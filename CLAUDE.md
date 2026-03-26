# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

A tmux plugin (installed via [TPM](https://github.com/tmux-plugins/tpm)) for people who run Claude Code across many tmux sessions and windows. The problem: you lose track of what Claude is doing in other sessions. This plugin gives you a single fzf popup (`prefix + S`) that lists every running Claude Code instance with its status (idle/working/waiting) and lets you jump to it.

It works by correlating Claude Code's internal session files, the OS process tree, and tmux pane metadata — no hooks or configuration needed beyond installing the plugin.

## Build & Test

```bash
go build -o bin/tmux-claude-picker .
go test ./...
go test -run Test_getClaudeDir ./...
./bin/tmux-claude-picker --debug   # prints entries to stdout without fzf
```

The binary is auto-built by `scripts/claude-picker.sh` when source files are newer than it.
