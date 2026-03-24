#!/usr/bin/env bash
# Launcher script for the Go-based Claude Code session picker for Tmux.
# Builds the binary on first run or when source changes, then executes it.

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(dirname "$CURRENT_DIR")"
BINARY="${PLUGIN_DIR}/bin/tmux-claude-picker"
SOURCE="${PLUGIN_DIR}/main.go"

# Build if binary doesn't exist or source is newer
if [ ! -f "$BINARY" ] || [ "$SOURCE" -nt "$BINARY" ]; then
  mkdir -p "${PLUGIN_DIR}/bin"
  if ! go build -o "$BINARY" "$SOURCE" 2>/tmp/claude-picker-build.log; then
    tmux display-message "tmux-claude-picker build failed: $(cat /tmp/tmux-claude-picker-build.log)"
    exit 1
  fi
fi

exec "$BINARY"
