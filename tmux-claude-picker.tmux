#!/usr/bin/env bash

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPTS_DIR="${CURRENT_DIR}/scripts"

# Default key binding: prefix + S
default_key="S"
key="$(tmux show-option -gqv @claude-picker-key)"
key="${key:-$default_key}"

tmux bind-key -N "Claude Code session picker" "$key" run-shell "${SCRIPTS_DIR}/claude-picker.sh"
