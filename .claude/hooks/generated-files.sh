#!/usr/bin/env bash
# PreToolUse hook: refuse an edit of a generated file. mise run generate
# writes these from their sources, and generate:check in CI fails on a hand
# edit. Exit code 2 refuses the tool call and gives the agent the message.
set -euo pipefail

command -v jq >/dev/null || exit 0
file=$(jq -r '.tool_input.file_path // empty')
[ -n "$file" ] || exit 0

case "$file" in
  */metal/config/crd/*|*/metal/config/rbac/role.yaml|*/docs/src/data/generated/*|*/zz_generated.deepcopy.go)
    echo "$file is generated: change its source (API types, kubebuilder markers, pk-metal commands, glossary) and run 'mise run generate'." >&2
    exit 2
    ;;
esac
exit 0
