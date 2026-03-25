#!/usr/bin/env bash
# Launcher script for the Go-based Claude Code session picker for Tmux.
# Builds the binary on first run or when source changes, then executes it.

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(dirname "$CURRENT_DIR")"
BINARY="${PLUGIN_DIR}/bin/tmux-claude-picker"

# Build if binary doesn't exist or source files changed
needs_build=false
if [ ! -f "$BINARY" ]; then
  needs_build=true
else
  for f in "${PLUGIN_DIR}"/main.go "${PLUGIN_DIR}"/go.mod "${PLUGIN_DIR}"/go.sum; do
    if [ -f "$f" ] && [ "$f" -nt "$BINARY" ]; then
      needs_build=true
      break
    fi
  done
fi

if [ "$needs_build" = true ]; then
  mkdir -p "${PLUGIN_DIR}/bin"
  if ! (cd "$PLUGIN_DIR" && go build -o "$BINARY" .) 2>/tmp/claude-picker-build.log; then
    tmux display-message "tmux-claude-picker build failed (is Go installed?)"
    exit 1
  fi
fi

exec "$BINARY"
