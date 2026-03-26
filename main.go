// Package main implements tmux-claude-picker, a tmux plugin that discovers all
// running Claude Code sessions across tmux panes and presents them in an
// interactive fzf picker for quick switching.
//
// # Premise
// The picker determines <claude_dir> by checking the CLAUDE_CONFIG_DIR environment variable, falling back to
// ~/.claude if not set.
//
// # How it works
//
// The picker correlates three data sources to build its list:
//
//  1. Claude Code session files (~/<claude_dir>/sessions/*.json)
//  2. The OS process tree (via ps)
//  3. Tmux pane metadata (via tmux list-panes)
//
// For each Claude session file, it verifies the PID is alive, walks up the
// process tree to find the owning tmux pane, detects whether the session is
// running inside neovim, and reads the JSONL conversation log to determine
// the session's current status (idle, working, waiting for tool approval).
//
// # Claude Code session files
//
// Claude Code writes a JSON file per running instance at:
//
//	~/<claude_dir>/sessions/{PID}.json
//
// Each file contains:
//
//	{
//	  "pid": 12345,              // OS process ID of the Claude Code process
//	  "sessionId": "uuid-...",   // Unique session identifier
//	  "cwd": "/Users/foo/proj",  // Working directory where Claude was started
//	  "startedAt": 1711234567890 // Unix timestamp in milliseconds
//	}
//
// These files may persist after the process exits (stale), so we validate
// each PID against the live process tree before using it.
//
// # Claude Code conversation logs (JSONL)
//
// Each session's conversation is logged as newline-delimited JSON at:
//
//	~/<claude_dir>/projects/{encoded-cwd}/{sessionId}.jsonl
//
// Where {encoded-cwd} is the working directory with "/" replaced by "-",
// e.g. "/Users/foo/bar" becomes "-Users-foo-bar".
//
// Each line is a JSON object with a "type" field. The types relevant to
// status detection are:
//
//   - "user"      — A user message. Contains a "message" object with "role"
//     and "content". When the user interrupts a response, a
//     special entry is written with content containing
//     "[Request interrupted by user]".
//
//   - "assistant" — An assistant response. Contains a "message" object with
//     an optional "stop_reason" field:
//     "end_turn"  = finished responding (idle)
//     "tool_use"  = wants to run a tool, awaiting approval (waiting)
//     nil/absent  = still streaming (working)
//
//   - "system"    — System metadata. The "subtype" field distinguishes:
//     "turn_duration"    = marks the end of a turn (idle)
//     "stop_hook_summary" = hook execution finished (idle)
//
//   - "progress"  — Progress updates during tool execution. The "data.type"
//     field may be "hook_progress" (which we skip past) or
//     other values indicating active work.
//
//   - "file-history-snapshot", "queue-operation" — Internal bookkeeping
//     entries that don't indicate session state; skipped during scanning.
//
// We read the last ~20 lines and scan newest-first to determine the current
// state as one of: "idle", "working", or "waiting".
//
// # Process tree walking
//
// Claude Code runs as a Node.js process, which may be a child of a shell,
// which may be a child of nvim (if using a terminal plugin), which is
// ultimately a child of the tmux pane's initial process. We walk up the
// process tree from the Claude PID until we find a PID that matches a tmux
// pane. Along the way, we check if any ancestor is nvim to annotate the
// entry with [nvim].
//
// # Display format
//
// Entries are formatted as tab-separated lines for fzf:
//
//	{pane_target}\t  {session}   {window}   {status}   {last_attached}   {elapsed}  {context}
//
// The pane_target (e.g. "mysession:0.1") is hidden from display via fzf's
// --with-nth=2.. but used as the switch target when selected. Column widths
// are computed using Unicode display width (via go-runewidth) to correctly
// align entries containing emojis or CJK characters.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

const (
	CLAUDE_CONFIG_DIR_ENV = "CLAUDE_CONFIG_DIR"
)

// PaneInfo holds metadata for a single tmux pane, extracted from the output
// of "tmux list-panes -a". The Target field uses tmux's standard addressing
// format "session_name:window_index.pane_index".
type PaneInfo struct {
	Target       string // e.g. "mysession:0.1"
	Session      string // e.g. "mysession"
	Window       string // e.g. "editor" (tmux window name)
	LastAttached int64  // unix timestamp of the session's last client attachment
}

// SessionJSON represents the contents of a Claude Code session file at
// ~/<claude_dir>/sessions/{PID}.json. See package doc for the full format.
type SessionJSON struct {
	PID       int    `json:"pid"`       // OS process ID of the Claude Code process
	SessionID string `json:"sessionId"` // UUID identifying the conversation session
	CWD       string `json:"cwd"`       // Working directory where Claude Code was launched
	StartedAt int64  `json:"startedAt"` // Session start time as Unix milliseconds
}

// ClaudeInstance represents a fully resolved Claude Code session: a session
// file that has been validated against the process tree, mapped to a tmux
// pane, and had its status detected from the conversation log.
type ClaudeInstance struct {
	PID          int
	SessionID    string
	CWD          string
	StartedAt    time.Time
	PaneTarget   string // tmux target for switching, e.g. "mysession:0.1"
	SessionName  string // tmux session name for display
	WindowName   string // tmux window name for display
	LastAttached int64  // for sorting: most recently attached first
	InNvim       bool   // true if Claude is running inside a neovim terminal
	Status       string // "working", "idle", or "waiting"
}

func main() {
	// Step 1: Build a lookup from tmux pane PIDs to pane metadata.
	paneLookup, err := buildPaneLookup()
	if err != nil {
		tmuxMessage("claude-picker:" + err.Error())
		os.Exit(1)
	}

	// Step 2: Snapshot the entire OS process tree for parent/child walking.
	parentOf, commOf, err := buildProcessTree()
	if err != nil {
		tmuxMessage("claude-picker:" + err.Error())
		os.Exit(1)
	}

	// Step 3: Read all Claude Code session files.
	sessions, err := readActiveSessions()
	if err != nil || len(sessions) == 0 {
		tmuxMessage("No Claude Code instances found")
		return
	}

	// Step 4: For each session, validate it's alive and map it to a tmux pane.
	seen := map[string]bool{}
	var instances []ClaudeInstance

	for _, s := range sessions {
		// Skip stale session files whose PID no longer exists.
		if _, ok := parentOf[s.PID]; !ok {
			continue
		}

		// Walk up the process tree to find the tmux pane that owns this process.
		panePID, found := walkToPane(s.PID, parentOf, paneLookup)
		if !found {
			continue
		}

		// Deduplicate: one entry per tmux pane even if multiple session files
		// point to the same pane (can happen with stale files).
		pane := paneLookup[panePID]
		if seen[pane.Target] {
			continue
		}
		seen[pane.Target] = true

		inst := ClaudeInstance{
			PID:          s.PID,
			SessionID:    s.SessionID,
			CWD:          s.CWD,
			StartedAt:    time.UnixMilli(s.StartedAt),
			PaneTarget:   pane.Target,
			SessionName:  pane.Session,
			WindowName:   pane.Window,
			LastAttached: pane.LastAttached,
			InNvim:       hasNvimAncestor(s.PID, panePID, parentOf, commOf),
			Status:       detectStatus(s.SessionID, s.CWD),
		}
		instances = append(instances, inst)
	}

	if len(instances) == 0 {
		tmuxMessage("No Claude Code instances found")
		return
	}

	// Sort by most recently attached tmux session first.
	sort.Slice(instances, func(i, j int) bool {
		return instances[i].LastAttached > instances[j].LastAttached
	})

	columns := getVisibleColumns()
	entries, header := formatEntries(instances, columns)

	// Debug mode: print entries to stdout instead of launching fzf.
	if len(os.Args) > 1 && os.Args[1] == "--debug" {
		fmt.Println(header)
		for _, e := range entries {
			fmt.Println(strings.Replace(e, "\t", " | ", 1))
		}
		return
	}

	selected, err := runFzfPicker(entries, header)
	if err != nil || selected == "" {
		return
	}

	// The pane target is the first tab-separated field.
	target := strings.SplitN(selected, "\t", 2)[0]
	exec.Command("tmux", "switch-client", "-t", target).Run()
}

// buildPaneLookup queries tmux for all panes across all sessions and returns
// a map from each pane's shell PID to its metadata.
//
// It runs: tmux list-panes -a -F "#{pane_pid}|#{session_name}|#{window_index}|#{pane_index}|#{session_last_attached}|#{window_name}"
//
// The output is pipe-delimited with one line per pane, e.g.:
//
//	12345|mysession|0|1|1711234567|editor
func buildPaneLookup() (map[int]PaneInfo, error) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_pid}|#{session_name}|#{window_index}|#{pane_index}|#{session_last_attached}|#{window_name}").Output()
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes failed: %w", err)
	}

	lookup := map[int]PaneInfo{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 6)
		if len(parts) < 6 {
			continue
		}
		pid, _ := strconv.Atoi(parts[0])
		lastAtt, _ := strconv.ParseInt(parts[4], 10, 64)
		lookup[pid] = PaneInfo{
			Target:       fmt.Sprintf("%s:%s.%s", parts[1], parts[2], parts[3]),
			Session:      parts[1],
			Window:       parts[5],
			LastAttached: lastAtt,
		}
	}
	return lookup, nil
}

// buildProcessTree snapshots the OS process tree by running "ps -axo pid,ppid,comm".
// It returns two maps:
//   - parentOf: maps each PID to its parent PID
//   - commOf:   maps each PID to its command name (used for nvim detection)
func buildProcessTree() (parentOf map[int]int, commOf map[int]string, err error) {
	out, err := exec.Command("ps", "-axo", "pid,ppid,comm").Output()
	if err != nil {
		return nil, nil, fmt.Errorf("ps failed: %w", err)
	}

	parentOf = map[int]int{}
	commOf = map[int]string{}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		parentOf[pid] = ppid
		commOf[pid] = fields[2]
	}
	return parentOf, commOf, nil
}

// readActiveSessions reads all Claude Code session files from
// ~/<claude_dir>/sessions/*.json and returns them as parsed structs.
// Files that fail to read or parse are silently skipped (they may be
// partially written or from an incompatible version).
func readActiveSessions() ([]SessionJSON, error) {
	claudeDir, err := getClaudeDir()
	if err != nil {
		return nil, err
	}

	pattern := filepath.Join(claudeDir, "sessions", "*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	var sessions []SessionJSON
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s SessionJSON
		if json.Unmarshal(data, &s) == nil && s.PID > 0 {
			sessions = append(sessions, s)
		}
	}
	return sessions, nil
}

// getClaudeDir returns the path to the directory where Claude Code stores files.
// getClaudeDir also respects CLAUDE_CONFIG_DIR environment variable if set, falling back to
// ~/.claude if not.
func getClaudeDir() (string, error) {
	if configDir := os.Getenv(CLAUDE_CONFIG_DIR_ENV); configDir != "" {
		return configDir, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".claude"), nil
}

// walkToPane walks from the given pid up the process tree (via parentOf)
// until it finds a PID that is a tmux pane shell process (present in
// paneLookup). Returns the pane PID and true if found, or (0, false) if the
// walk reaches PID 1 or a broken chain without finding a pane.
//
// The typical chain looks like:
//
//	claude (node) → bash/zsh → [nvim → bash/zsh →] tmux-pane-shell
func walkToPane(pid int, parentOf map[int]int, paneLookup map[int]PaneInfo) (int, bool) {
	current := pid
	for current > 1 {
		if _, ok := paneLookup[current]; ok {
			return current, true
		}
		ppid, ok := parentOf[current]
		if !ok {
			break
		}
		current = ppid
	}
	return 0, false
}

// hasNvimAncestor checks whether any process in the chain between pid
// (exclusive) and panePID (exclusive) has "nvim" in its command name.
// This is used to annotate picker entries with [nvim] so the user can
// distinguish Claude sessions running in a neovim terminal buffer from
// standalone ones.
func hasNvimAncestor(pid, panePID int, parentOf map[int]int, commOf map[int]string) bool {
	current := pid
	for current != panePID && current > 1 {
		if strings.Contains(commOf[current], "nvim") {
			return true
		}
		ppid, ok := parentOf[current]
		if !ok {
			break
		}
		current = ppid
	}
	return false
}

// detectStatus determines the current state of a Claude Code session by
// reading the tail of its JSONL conversation log.
//
// The log path is derived from the session's working directory and ID:
//
//	~/<claude_dir>/projects/{encoded-cwd}/{sessionId}.jsonl
//
// Where {encoded-cwd} replaces all "/" with "-", e.g.:
//
//	cwd "/Users/foo/bar" → encoded "-Users-foo-bar"
//
// Returns "idle", "working", or "waiting".
func detectStatus(sessionID, cwd string) string {
	claudeDir, err := getClaudeDir()
	if err != nil {
		return "idle"
	}

	encodedPath := strings.ReplaceAll(cwd, "/", "-")
	jsonlPath := filepath.Join(claudeDir, "projects", encodedPath, sessionID+".jsonl")

	lines, err := readLastLines(jsonlPath, 20)
	if err != nil {
		// File doesn't exist yet (brand new session) → waiting for user input.
		return "idle"
	}

	return classifyStatus(lines)
}

// readLastLines reads the last n complete lines from a file by scanning
// backward from the end in chunks. Lines are returned in reverse order
// (newest first).
//
// This approach handles arbitrarily long lines (e.g. JSONL entries containing
// large tool results or base64 images) without loading the entire file, since
// we only need a small number of recent entries for status classification.
func readLastLines(path string, n int) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	size := fi.Size()
	if size == 0 {
		return nil, nil
	}

	// Scan backward in chunks, collecting newline byte positions.
	const chunkSize = 4096
	var newlines []int64
	pos := size

	for pos > 0 && len(newlines) <= n {
		readSize := int64(chunkSize)
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize

		buf := make([]byte, readSize)
		_, err := f.ReadAt(buf, pos)
		if err != nil && err != io.EOF {
			return nil, err
		}

		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				newlines = append(newlines, pos+int64(i))
			}
		}
	}

	// Extract lines between newline positions (newest first).
	var lines [][]byte
	end := size
	for _, nlPos := range newlines {
		lineStart := nlPos + 1
		if lineStart >= end {
			end = nlPos
			continue // empty line
		}
		lineBuf := make([]byte, end-lineStart)
		f.ReadAt(lineBuf, lineStart)
		lines = append(lines, bytes.TrimSpace(lineBuf))
		end = nlPos
		if len(lines) >= n {
			break
		}
	}

	// If we reached the start of the file, include the first line.
	if end > 0 && len(lines) < n {
		lineBuf := make([]byte, end)
		f.ReadAt(lineBuf, 0)
		lines = append(lines, bytes.TrimSpace(lineBuf))
	}

	return lines, nil
}

// classifyStatus examines JSONL entries (in reverse chronological order,
// newest first) and returns the session's current state.
//
// The logic scans backward through entries looking for the first one that
// definitively indicates a state:
//
//   - "idle": The assistant finished its turn (stop_reason="end_turn"),
//     a system turn_duration/stop_hook_summary was logged, or the user
//     interrupted the response.
//
//   - "waiting": The assistant emitted a tool_use stop_reason, meaning it
//     wants to execute a tool and is waiting for user approval.
//
//   - "working": A user message was sent (Claude is processing), an
//     assistant message is streaming (no stop_reason yet), or a non-hook
//     progress event is active.
//
// Metadata-only entries (file-history-snapshot, queue-operation, hook_progress)
// are skipped as they don't indicate conversational state.
func classifyStatus(lines [][]byte) string {
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if len(line) == 0 {
			continue
		}

		var entry struct {
			Type    string          `json:"type"`
			Subtype string          `json:"subtype"`
			Message json.RawMessage `json:"message"`
			Data    struct {
				Type string `json:"type"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}

		switch entry.Type {
		case "system":
			if entry.Subtype == "turn_duration" || entry.Subtype == "stop_hook_summary" {
				return "idle"
			}

		case "assistant":
			if len(entry.Message) > 0 {
				var msg struct {
					StopReason *string `json:"stop_reason"`
				}
				json.Unmarshal(entry.Message, &msg)
				if msg.StopReason != nil {
					switch *msg.StopReason {
					case "end_turn":
						return "idle"
					case "tool_use":
						return "waiting"
					}
				}
			}
			return "working"

		case "user":
			// When the user interrupts a response, Claude Code writes a user
			// entry with content "[Request interrupted by user]". The session
			// is back at the prompt, so this counts as idle.
			if bytes.Contains(entry.Message, []byte("Request interrupted by user")) {
				return "idle"
			}
			return "working"

		case "progress":
			// Hook progress events are informational; skip them and keep
			// scanning for the actual conversational state.
			if entry.Data.Type == "hook_progress" {
				continue
			}
			return "working"

		case "file-history-snapshot", "queue-operation":
			continue // internal bookkeeping, not conversational state
		}
	}

	// No recognizable state entries found (e.g. brand new session with only
	// metadata lines, or empty file) → treat as idle since it's awaiting input.
	return "idle"
}

// padRight pads s with trailing spaces until its terminal display width
// equals targetWidth. Uses go-runewidth to correctly handle characters that
// occupy more than one column (emojis, CJK characters, etc.).
func padRight(s string, targetWidth int) string {
	w := runewidth.StringWidth(s)
	if w >= targetWidth {
		return s
	}
	return s + strings.Repeat(" ", targetWidth-w)
}

// getVisibleColumns reads the @claude-picker-columns tmux option to determine
// which columns to display. The option is a comma-separated list of column
// names. If unset, all columns are shown.
//
// Available columns: session, window, status, ago, elapsed, context
//
// Example tmux config:
//
//	set -g @claude-picker-columns "window,status,elapsed"
func getVisibleColumns() map[string]bool {
	defaults := []string{"session", "window", "status", "ago", "elapsed", "context"}

	out, err := exec.Command("tmux", "show-option", "-gqv", "@claude-picker-columns").Output()
	if err != nil {
		// Not in tmux or option not set — show all columns.
		cols := map[string]bool{}
		for _, c := range defaults {
			cols[c] = true
		}
		return cols
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		cols := map[string]bool{}
		for _, c := range defaults {
			cols[c] = true
		}
		return cols
	}

	cols := map[string]bool{}
	for _, c := range strings.Split(raw, ",") {
		cols[strings.TrimSpace(c)] = true
	}
	return cols
}

// columnDef defines a picker column: its config key, display header, and
// the maximum display width needed across all rows (computed at format time).
type columnDef struct {
	key    string
	header string
}

// allColumns defines every available column in display order.
var allColumns = []columnDef{
	{"session", "SESSION"},
	{"window", "WINDOW"},
	{"status", "STATUS"},
	{"ago", "ATTACHED"},
	{"elapsed", "ELAPSED"},
	{"context", "CONTEXT"},
}

// formatEntries builds fzf-compatible display strings from the resolved
// instances. Each entry is a tab-separated line where the pane_target is
// hidden from the fzf display (via --with-nth=2..) but preserved in the
// output for switching. Which columns appear is controlled by the columns
// parameter. Columns are padded to align using terminal display width
// rather than byte length.
//
// Returns the formatted entries and a header string for fzf --header.
func formatEntries(instances []ClaudeInstance, columns map[string]bool) ([]string, string) {
	now := time.Now()

	type formatted struct {
		session, window, status, ago, elapsed, context string
	}
	rows := make([]formatted, len(instances))

	// Compute column values and track max widths (keyed by column key).
	maxWidth := map[string]int{}

	for i, inst := range instances {
		ago := formatAgo(now.Unix() - inst.LastAttached)
		elapsed := formatElapsed(now.Sub(inst.StartedAt))
		ctx := ""
		if inst.InNvim {
			ctx = "[nvim]"
		}

		status := formatStatus(inst.Status)
		rows[i] = formatted{inst.SessionName, inst.WindowName, status, ago, elapsed, ctx}

		vals := map[string]string{
			"session": inst.SessionName,
			"window":  inst.WindowName,
			"status":  status,
			"ago":     ago,
			"elapsed": elapsed,
			"context": ctx,
		}
		for k, v := range vals {
			if w := runewidth.StringWidth(v); w > maxWidth[k] {
				maxWidth[k] = w
			}
		}
	}

	// Ensure header labels are accounted for in column widths.
	for _, col := range allColumns {
		if !columns[col.key] {
			continue
		}
		if w := runewidth.StringWidth(col.header); w > maxWidth[col.key] {
			maxWidth[col.key] = w
		}
	}

	// Build header.
	var headerParts []string
	for _, col := range allColumns {
		if !columns[col.key] {
			continue
		}
		headerParts = append(headerParts, padRight(col.header, maxWidth[col.key]))
	}
	header := "  " + strings.Join(headerParts, "   ")

	// Build entries.
	valFor := func(r formatted, key string) string {
		switch key {
		case "session":
			return r.session
		case "window":
			return r.window
		case "status":
			return r.status
		case "ago":
			return r.ago
		case "elapsed":
			return r.elapsed
		case "context":
			return r.context
		}
		return ""
	}

	entries := make([]string, len(instances))
	for i, inst := range instances {
		r := rows[i]
		var parts []string
		for _, col := range allColumns {
			if !columns[col.key] {
				continue
			}
			parts = append(parts, padRight(valFor(r, col.key), maxWidth[col.key]))
		}
		entries[i] = inst.PaneTarget + "\t  " + strings.Join(parts, "   ")
	}
	return entries, header
}

// formatStatus prepends a status emoji for quick visual scanning.
func formatStatus(status string) string {
	switch status {
	case "idle":
		return "💤 idle"
	case "working":
		return "🔨 working"
	case "waiting":
		return "⏳ waiting"
	default:
		return status
	}
}

// formatElapsed converts a duration to a compact human-readable string.
// Examples: "<1m", "5m", "2h30m", "1d14h30m".
func formatElapsed(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	var result string
	if days > 0 {
		result += fmt.Sprintf("%dd", days)
	}
	if hours > 0 {
		result += fmt.Sprintf("%dh", hours)
	}
	if mins > 0 {
		result += fmt.Sprintf("%dm", mins)
	}
	if result == "" {
		result = "<1m"
	}
	return result
}

// formatAgo converts a delta in seconds to a human-readable "time ago" string.
// Examples: "just now", "3m ago", "2h15m ago", "5d ago".
func formatAgo(delta int64) string {
	switch {
	case delta < 60:
		return "just now"
	case delta < 3600:
		return fmt.Sprintf("%dm ago", delta/60)
	case delta < 86400:
		h := delta / 3600
		m := (delta % 3600) / 60
		if m > 0 {
			return fmt.Sprintf("%dh%dm ago", h, m)
		}
		return fmt.Sprintf("%dh ago", h)
	default:
		return fmt.Sprintf("%dd ago", delta/86400)
	}
}

// runFzfPicker launches fzf-tmux as a floating popup and returns the
// user-selected line. Returns empty string and nil error if the user
// cancels (presses Escape).
func runFzfPicker(entries []string, header string) (string, error) {
	cmd := exec.Command("fzf-tmux",
		"-p", "65%,40%",
		"--no-sort", "--ansi", "--layout=reverse",
		"--border-label", " Claude Code ",
		"--prompt", "🤖  ",
		"--header", header,
		"--color", "fg+:#cdd6f4,bg+:#45475a,hl+:#f38ba8,pointer:#f5e0dc,gutter:-1",
		"--bind", "tab:down,btab:up",
		"--with-nth=2..",
		"--delimiter=\t",
	)

	cmd.Stdin = strings.NewReader(strings.Join(entries, "\n"))
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// tmuxMessage displays a short message in the tmux status line.
func tmuxMessage(msg string) {
	exec.Command("tmux", "display-message", msg).Run()
}
