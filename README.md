# tmux-claude-picker

A tmux plugin built on [fzf-tmux](https://github.com/junegunn/fzf#fzf-tmux) to find and switch between running Claude Code sessions. Shows live status (working/idle), session name, uptime, and whether Claude is running inside nvim.

## Features

- Lists all active Claude Code instances across tmux sessions/windows
- Detects session state by reading Claude Code's internal session files — no hooks or extra config needed
- Shows status: `working` (actively generating/executing tools), `idle` (waiting for input), `?` (unknown)
- Detects nvim-hosted Claude instances
- Sorted by most recently visited tmux session
- fzf-tmux popup picker with one-key switching

## Requirements

- Go (for building the binary)
- [fzf](https://github.com/junegunn/fzf)
- [TPM](https://github.com/tmux-plugins/tpm)

## Install

Add to `tmux.conf`:

```tmux
set -g @plugin 'lugen4ro/tmux-claude-picker'
```

Then `prefix + I` to install via TPM.

The Go binary is built automatically on first use.

## Usage

`prefix + S` opens the picker.

### Configuration

Change the key binding:

```tmux
set -g @claude-picker-key 'C-s'
```

## How It Works

1. Reads `~/.claude/sessions/*.json` to discover all active Claude Code instances (PID, session ID, working directory)
2. Queries tmux for all panes and walks the process tree to map each Claude PID to its tmux pane
3. Reads the tail of each session's JSONL transcript (`~/.claude/projects/{path}/{sessionId}.jsonl`) to classify state from the last few log entries
4. Presents results in an fzf-tmux popup, switches to the selected pane

## Acknowledgements

- [fzf](https://github.com/junegunn/fzf) / [fzf-tmux](https://github.com/junegunn/fzf#fzf-tmux) — powers the picker UI
- [TPM](https://github.com/tmux-plugins/tpm) — plugin distribution
