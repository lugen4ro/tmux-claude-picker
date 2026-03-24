# Status Detection: Approaches Considered

When building a tmux picker for Claude Code sessions, the core challenge is determining each session's state: is Claude actively working, waiting for user input, or encountering an error?

We evaluated four approaches before settling on JSONL transcript reading.

## Approach 1: Process-based detection

**Idea:** Use OS-level process information (CPU usage, child processes, process age) to infer whether Claude is active.

**Pros:**

- No dependency on Claude Code internals
- Works regardless of Claude Code version

**Cons:**

- CPU usage is unreliable — network I/O (API calls) doesn't show as CPU activity, so Claude can be "working" at 0% CPU
- Child process detection only catches tool execution (bash, git), not thinking/generating phases
- No way to distinguish "idle waiting for input" from "idle between API calls"

**Verdict:** Too coarse. You can tell if something is running, but not what state it's in.

## Approach 2: Claude Code hooks

**Idea:** Configure hooks in `~/.claude/settings.json` that write state to files on lifecycle events (`PreToolUse`, `Stop`, `StopFailure`, `PermissionRequest`, etc.).

```json
{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "echo idle > /tmp/claude-state/$SESSION_ID"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "echo working > /tmp/claude-state/$SESSION_ID"
          }
        ]
      }
    ]
  }
}
```

**Pros:**

- Official, supported mechanism
- Real-time state transitions
- Can distinguish many states (working, idle, waiting for permission, error)

**Cons:**

- Requires user configuration — every user must add hooks to their settings
- Introduces new state files that need cleanup
- Hook commands receive JSON on stdin requiring parsing for the session ID
- Adds overhead to every tool call and turn completion
- Conflicts with existing hooks need careful management

**Verdict:** Powerful but invasive. Requires setup and introduces external state.

## Approach 3: StatusLine hook

**Idea:** Use Claude Code's `statusLine` hook, which fires after assistant messages and provides JSON with context window usage, rate limits, cost, and model info.

**Pros:**

- Rich metadata (context %, cost, rate limits)
- Official feature

**Cons:**

- Does NOT include state information (working/idle/waiting) — only metrics
- Only fires after assistant messages, not on state transitions
- Debounced at 300ms
- Same configuration burden as hooks

**Verdict:** Useful for metrics but doesn't solve the state detection problem.

## Approach 4: JSONL transcript reading (chosen)

**Idea:** Claude Code already maintains a live conversation transcript at `~/.claude/projects/{encoded-path}/{sessionId}.jsonl`. Each line is a JSON object with a `type` field. By reading the last few entries, you can classify the session state.

**State classification from last JSONL entry:**

| Entry                                                               | State   |
| ------------------------------------------------------------------- | ------- |
| `type:"system"`, `subtype:"turn_duration"` or `"stop_hook_summary"` | idle    |
| `type:"assistant"`, `stop_reason:"end_turn"`                        | idle    |
| `type:"assistant"`, `stop_reason:"tool_use"` or `null`              | working |
| `type:"user"` (tool result or new message)                          | working |
| `type:"progress"` / `"agent_progress"`                              | working |

Combined with `~/.claude/sessions/{PID}.json` (which maps running PIDs to session IDs and working directories), this gives us everything without any configuration.

**Pros:**

- Zero configuration — works out of the box
- No new files or state to manage
- Reads only the last ~8KB of the file (seek to end, read backward) — fast even for large sessions
- Accurate state detection matching what Claude is actually doing
- `sessions/*.json` only contains active sessions, eliminating the need to scan processes for Claude PIDs

**Cons:**

- Depends on Claude Code's internal file format, which could change between versions
- Cannot distinguish "waiting for user text input" from "session still open but user walked away" (both show as `idle`)
- Less granular than hooks — can't detect `PermissionRequest` specifically (shows as `working`)

**Verdict:** The right trade-off for a tmux picker. Zero setup, fast, accurate enough for the use case, and relies on files Claude Code already writes.

## Why we chose JSONL reading

The deciding factors were:

1. **Zero configuration** — a tmux plugin should work after `prefix + I`, not require editing `settings.json`
2. **No new state** — leveraging files that already exist avoids cleanup problems and race conditions
3. **Sufficient granularity** — for a quick-glance picker, knowing "working" vs "idle" covers the primary need of "do I need to go look at this session?"
4. **Performance** — reading 8KB from the tail of a local file is effectively instant
