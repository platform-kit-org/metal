#!/usr/bin/env bash
# Model-checks the TLA+ specifications in spec/ with TLC.
#
# For each specification directory (spec/<name>/, with MC.tla):
#   - every MC*.cfg whose name does not end in _cache.cfg must pass;
#   - MC_cache.cfg, if there is one, must FAIL with the property its header
#     names: it holds a derivation from before a fix, and the failure proves
#     that the property catches the defect;
#   - every Never* operator of MC.tla must be violated: each names a state the
#     model must reach. A model that cannot reach the states the invariants
#     are about proves nothing.
#
# Java is a system package (apt-get install default-jre-headless). The TLA+
# tools jar is downloaded once to bin/tla/, at a pinned version and checksum.
set -euo pipefail

SCRIPT=${0##*/}
HELP="usage: $SCRIPT [spec directory ...]   (run from metal/; default: every directory in spec/)"

TLA_VERSION=1.7.4
TLA_SHA256=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88
JAR=bin/tla/tla2tools-$TLA_VERSION.jar

die() {
  echo "Error: $*" >&2
  exit 1
}

case ${1:-} in
-h | --help)
  echo "$HELP"
  exit 0
  ;;
esac

[ -d spec ] || die "run from metal/: there is no spec/ here"
command -v java >/dev/null || die "java is not on PATH; install a JRE 11 or later (apt-get install default-jre-headless)"

if [ ! -s "$JAR" ] || ! echo "$TLA_SHA256  $JAR" | sha256sum -c --status; then
  mkdir -p "${JAR%/*}"
  curl -fsSL --retry 3 -o "$JAR.tmp" \
    "https://github.com/tlaplus/tlaplus/releases/download/v$TLA_VERSION/tla2tools.jar"
  echo "$TLA_SHA256  $JAR.tmp" | sha256sum -c --status || die "checksum mismatch for tla2tools.jar $TLA_VERSION"
  mv "$JAR.tmp" "$JAR"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# tlc runs TLC on dir/MC.tla with config cfg; its output goes to $work/out.
tlc() {
  local dir=$1 cfg=$2
  java -XX:+UseParallelGC -cp "$JAR" tlc2.TLC -workers auto -deadlock -cleanup \
    -metadir "$work/states" -config "$cfg" "$dir/MC.tla" >"$work/out" 2>&1
}

# summary prints TLC's verdict and the size of the search.
summary() {
  grep -E 'is violated|No error has been found|distinct states found|depth of the complete' "$work/out" | sed 's/^/    /'
}

failed=0
fail() {
  echo "  FAIL: $*"
  failed=1
}

dirs=("$@")
[ ${#dirs[@]} -gt 0 ] || dirs=(spec/*/)
for dir in "${dirs[@]}"; do
  dir=${dir%/}
  [ -f "$dir/MC.tla" ] || die "$dir has no MC.tla"
  echo "== $dir"

  for cfg in "$dir"/MC*.cfg; do
    case $cfg in
    *_cache.cfg)
      # The property to break is the one the config lists.
      want=$(sed -n 's/^PROPERTIES *//p; s/^INVARIANTS *//p' "$cfg" | head -1)
      echo "  ${cfg##*/}: must violate $want"
      if tlc "$dir" "$cfg"; then
        fail "${cfg##*/}: no violation of $want. If the derivation of main is now this one, the property is gone: check the model."
      elif ! grep -q "$want is violated" "$work/out"; then
        cat "$work/out"
        fail "${cfg##*/}: TLC failed, but not with a violation of $want"
      fi
      summary
      ;;
    *)
      echo "  ${cfg##*/}: must pass"
      if ! tlc "$dir" "$cfg"; then
        cat "$work/out"
        fail "${cfg##*/}"
      fi
      summary
      ;;
    esac
  done

  # Reachability: each Never* operator, as the only invariant of the first
  # config that can reach it, must be violated.
  for never in $(sed -n 's/^\(Never[A-Za-z0-9_]*\) *==.*/\1/p' "$dir/MC.tla"); do
    reached=
    for cfg in "$dir"/MC*.cfg; do
      case $cfg in *_cache.cfg) continue ;; esac
      grep -v -E '^(INVARIANTS|PROPERTIES)' "$cfg" >"$work/reach.cfg"
      echo "INVARIANT $never" >>"$work/reach.cfg"
      if ! tlc "$dir" "$work/reach.cfg" && grep -q "Invariant $never is violated" "$work/out"; then
        reached=${cfg##*/}
        break
      fi
    done
    if [ -n "$reached" ]; then
      echo "  reach ${never#Never}: yes ($reached)"
    else
      fail "reach ${never#Never}: no config reaches it; the model is too narrow to check what it claims"
    fi
  done
done

[ $failed -eq 0 ] || exit 1
echo "$SCRIPT: all specifications hold"
