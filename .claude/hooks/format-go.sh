#!/usr/bin/env bash
# PostToolUse hook: format a Go file of the module after an edit, as lint:go
# asks for (gofmt, and goimports with the module as the local prefix).
set -euo pipefail

command -v jq >/dev/null || exit 0
file=$(jq -r '.tool_input.file_path // empty')
case "$file" in
  *.go) ;;
  *) exit 0 ;;
esac
[ -f "$file" ] || exit 0
module="${CLAUDE_PROJECT_DIR:-$(git -C "$(dirname "$file")" rev-parse --show-toplevel)}/metal"
case "$file" in
  "$module"/*) ;;
  *) exit 0 ;;
esac

if command -v golangci-lint >/dev/null; then
  (cd "$module" && golangci-lint fmt "$file") || true
elif command -v gofmt >/dev/null; then
  gofmt -w "$file" || true
fi
exit 0
