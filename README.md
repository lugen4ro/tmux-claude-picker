# tmux-claude-picker

A tmux plugin for managing multiple Claude Code sessions. If you run Claude Code across many tmux sessions, this gives you a single picker to see all of them at a glance and jump directly to any one.

Built on [fzf-tmux](https://github.com/junegunn/fzf#fzf-tmux).

![screenshot](https://github.com/user-attachments/assets/placeholder.png)

<img width="2606" height="1461" alt="CleanShot2026-03-25at13 20 12" src="https://github.com/user-attachments/assets/08110d13-a53e-4b07-a2a3-be60b1041322" />


## Features

- Lists all active Claude Code instances across all tmux sessions and windows
- Shows live status: `working` (generating/executing), `idle` (waiting for input), `waiting` (needs tool approval)
- Detects nvim-hosted Claude instances (shown with `[nvim]` tag)
- Sorted by most recently visited tmux session
- One-key switching via fzf popup

No hooks or extra configuration needed — status is detected by reading Claude Code's internal session files.

## Requirements

- [Go](https://go.dev/) (for building from source)
- [fzf](https://github.com/junegunn/fzf)
- [TPM](https://github.com/tmux-plugins/tpm)

## Install

Add to `tmux.conf`:

```tmux
set -g @plugin 'lugen4ro/tmux-claude-picker'
```

Then `prefix + I` to install via TPM. The Go binary is built automatically on first use.

## Usage

`prefix + S` (capital S, for Claude's **S**essions) opens the picker. Select a session and press Enter to switch to it.

### Configuration

Change the key binding:

```tmux
set -g @claude-picker-key 'C-s'
```

## How It Works

1. Reads `~/.claude/sessions/*.json` to discover running Claude Code instances
2. Walks the OS process tree to map each Claude process to its tmux pane
3. Reads the tail of each session's JSONL conversation log to determine status
4. Presents everything in an fzf-tmux popup and switches to the selected pane

## Acknowledgements

- [fzf](https://github.com/junegunn/fzf) / [fzf-tmux](https://github.com/junegunn/fzf#fzf-tmux) — picker UI
- [TPM](https://github.com/tmux-plugins/tpm) — plugin distribution
