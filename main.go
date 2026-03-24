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
)

// PaneInfo holds tmux pane metadata.
type PaneInfo struct {
	Target       string // "session:window.pane"
	Session      string
	LastAttached int64 // unix timestamp
}

// SessionJSON is the structure of ~/.claude/sessions/{PID}.json.
type SessionJSON struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"` // unix millis
}

// ClaudeInstance is a discovered Claude session mapped to a tmux pane.
type ClaudeInstance struct {
	PID          int
	SessionID    string
	CWD          string
	StartedAt    time.Time
	PaneTarget   string
	SessionName  string
	LastAttached int64
	InNvim       bool
	Status       string // "working", "idle", "waiting", "?"
}

func main() {
	paneLookup, err := buildPaneLookup()
	if err != nil {
		tmuxMessage("claude-picker:" + err.Error())
		os.Exit(1)
	}

	parentOf, commOf, err := buildProcessTree()
	if err != nil {
		tmuxMessage("claude-picker:" + err.Error())
		os.Exit(1)
	}

	sessions, err := readActiveSessions()
	if err != nil || len(sessions) == 0 {
		tmuxMessage("No Claude Code instances found")
		return
	}

	seen := map[string]bool{}
	var instances []ClaudeInstance

	for _, s := range sessions {
		// Verify PID is actually in the process tree (not stale)
		if _, ok := parentOf[s.PID]; !ok {
			continue
		}

		panePID, found := walkToPane(s.PID, parentOf, paneLookup)
		if !found {
			continue
		}

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

	sort.Slice(instances, func(i, j int) bool {
		return instances[i].LastAttached > instances[j].LastAttached
	})

	entries := formatEntries(instances)

	// Debug mode: just print entries and exit
	if len(os.Args) > 1 && os.Args[1] == "--debug" {
		for _, e := range entries {
			// Replace tab with visible separator for debug
			fmt.Println(strings.Replace(e, "\t", " | ", 1))
		}
		return
	}

	selected, err := runFzfPicker(entries)
	if err != nil || selected == "" {
		return
	}

	target := strings.SplitN(selected, "\t", 2)[0]
	exec.Command("tmux", "switch-client", "-t", target).Run()
}

// buildPaneLookup queries tmux for all panes and returns a map of pane_pid -> PaneInfo.
func buildPaneLookup() (map[int]PaneInfo, error) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_pid}|#{session_name}|#{window_index}|#{pane_index}|#{session_last_attached}").Output()
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes failed: %w", err)
	}

	lookup := map[int]PaneInfo{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 5)
		if len(parts) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(parts[0])
		lastAtt, _ := strconv.ParseInt(parts[4], 10, 64)
		lookup[pid] = PaneInfo{
			Target:       fmt.Sprintf("%s:%s.%s", parts[1], parts[2], parts[3]),
			Session:      parts[1],
			LastAttached: lastAtt,
		}
	}
	return lookup, nil
}

// buildProcessTree runs a single ps command and returns parent/comm maps.
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

// readActiveSessions reads all ~/.claude/sessions/*.json files.
func readActiveSessions() ([]SessionJSON, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	pattern := filepath.Join(home, ".claude", "sessions", "*.json")
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

// walkToPane walks from pid up the process tree until it finds a tmux pane PID.
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

// hasNvimAncestor checks if any process between pid and panePID is nvim.
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

// detectStatus reads the JSONL tail to determine Claude's current state.
func detectStatus(sessionID, cwd string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "?"
	}

	// Encode path: /Users/foo/bar -> -Users-foo-bar
	encodedPath := strings.ReplaceAll(cwd, "/", "-")
	jsonlPath := filepath.Join(home, ".claude", "projects", encodedPath, sessionID+".jsonl")

	lines, err := readLastLines(jsonlPath, 20)
	if err != nil {
		// File doesn't exist yet (brand new session) → waiting for user input
		return "idle"
	}

	return classifyStatus(lines)
}

// readLastLines reads the last n complete lines from a file by scanning
// backward from the end. This handles arbitrarily long lines (e.g. large
// tool results) without needing to guess a byte budget.
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

	// Scan backward collecting newline positions
	const chunkSize = 4096
	var newlines []int64 // positions of '\n' characters
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

	// Determine start positions for each line
	// newlines are in reverse order (from end of file backward)
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

	// If we reached the start of the file, include the first line
	if end > 0 && len(lines) < n {
		lineBuf := make([]byte, end)
		f.ReadAt(lineBuf, 0)
		lines = append(lines, bytes.TrimSpace(lineBuf))
	}

	return lines, nil
}

// classifyStatus parses JSONL lines (in reverse order, newest first) and
// determines the session state.
func classifyStatus(lines [][]byte) string {
	// Lines are already in reverse order (newest first)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if len(line) == 0 {
			continue
		}

		var entry struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
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
			return "working"
		case "progress":
			if entry.Data.Type == "hook_progress" {
				continue // skip hook progress, keep scanning
			}
			return "working"
		case "file-history-snapshot", "queue-operation":
			continue // metadata, keep scanning
		}
	}
	// No recognizable state entries found (e.g. brand new session with only
	// metadata lines, or empty file) → treat as idle since it's awaiting input.
	return "idle"
}

// formatEntries builds fzf-compatible display strings.
func formatEntries(instances []ClaudeInstance) []string {
	now := time.Now()

	// Compute column widths
	maxSession, maxStatus, maxAgo, maxElapsed := 0, 0, 0, 0

	type formatted struct {
		session, status, ago, elapsed, context string
	}
	rows := make([]formatted, len(instances))

	for i, inst := range instances {
		ago := formatAgo(now.Unix() - inst.LastAttached)
		elapsed := formatElapsed(now.Sub(inst.StartedAt))
		ctx := ""
		if inst.InNvim {
			ctx = "[nvim]"
		}

		rows[i] = formatted{inst.SessionName, inst.Status, ago, elapsed, ctx}

		if len(inst.SessionName) > maxSession {
			maxSession = len(inst.SessionName)
		}
		if len(inst.Status) > maxStatus {
			maxStatus = len(inst.Status)
		}
		if len(ago) > maxAgo {
			maxAgo = len(ago)
		}
		if len(elapsed) > maxElapsed {
			maxElapsed = len(elapsed)
		}
	}

	entries := make([]string, len(instances))
	fmtStr := fmt.Sprintf("%%s\t  %%-%ds   %%-%ds   %%-%ds   %%-%ds  %%s",
		maxSession, maxStatus, maxAgo, maxElapsed)

	for i, inst := range instances {
		r := rows[i]
		entries[i] = fmt.Sprintf(fmtStr, inst.PaneTarget, r.session, r.status, r.ago, r.elapsed, r.context)
	}
	return entries
}

// formatElapsed converts a duration to a compact string like "2h30m".
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

// formatAgo converts a delta in seconds to "just now", "3m ago", etc.
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

// runFzfPicker spawns fzf-tmux and returns the selected line.
func runFzfPicker(entries []string) (string, error) {
	cmd := exec.Command("fzf-tmux",
		"-p", "65%,40%",
		"--no-sort", "--ansi", "--layout=reverse",
		"--border-label", " Claude Code ",
		"--prompt", "🤖  ",
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

// tmuxMessage displays a message in tmux.
func tmuxMessage(msg string) {
	exec.Command("tmux", "display-message", msg).Run()
}
