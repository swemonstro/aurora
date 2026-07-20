# Aurora

Aurora is an experimental physical status indicator for AI workers.

The project contains:

- `aurora-agent`: runs where Claude, Codex, or another supported AI tool runs.
- `aurora-relay`: distributes minimal AI presence snapshots over outbound TLS connections.
- `device`: firmware for the Aurora ESP device.

Aurora must not transmit prompts, source code, file names, terminal output, or other work content.

## Claude Code hooks

`aurora-claude-hook` reads the lifecycle event supplied by Claude Code on stdin and
publishes only its mapped presence state to Aurora Relay. It never publishes prompt
or session content. Delivery is best effort: malformed input, unsupported events,
and relay failures are ignored so the hook cannot interrupt Claude Code.

This describes normal operation. The optional raw capture mode documented below is
strictly for temporary diagnostics and writes the complete hook payload to local
disk instead of sending that raw payload to the relay.

| Claude Code event | Per-session effect |
| --- | --- |
| `UserPromptSubmit` | Set session to `working` |
| `Notification` | Set session to `attention` |
| `Stop` | Set session to `attention` |
| `StopFailure` | Set session to `error` |
| `SessionEnd` | Remove that session |

Observed Claude Code behavior is a `UserPromptSubmit` when work begins, followed by
`Stop` and an `idle_prompt` notification after a normal response. Permission prompts
arrive as notifications with `notification_type="permission_prompt"`; both those and
`idle_prompt` notifications map to attention. Other notification types currently do
the same. Session exit arrives as `SessionEnd`, commonly with a reason such as
`prompt_input_exit`.

### Concurrent session aggregation

Each invocation updates only its `session_id`. Aurora aggregates all active Claude
Code sessions using this priority:

```text
error > attention > working > idle
```

Ending one session therefore does not publish idle while another session is working
or needs attention. With no active sessions, the aggregate is idle. A missing
session ID is not persisted as a synthetic global session.

Hooks are global across Claude Code projects, so aggregation is keyed only by
`session_id`; it is deliberately not scoped by working directory, repository,
transcript path, or prompt ID. Any supported event for an unknown non-empty session
creates that session at the event's mapped state. `SessionEnd` only removes and
never creates a session.

After hooks are first installed (or after they were temporarily unavailable), an
already-running session may first appear as `Stop`, Notification, or `StopFailure`.
Aurora records the state implied by that first observed event, but cannot infer a
prior working state whose `UserPromptSubmit` was never observed.

The shared state defaults to:

```text
~/.local/state/aurora/claude-sessions.json
```

It contains only session IDs, mapped states, and last-updated timestamps. Prompts,
assistant responses, transcript paths, working directories, and other hook fields
are not retained. The directory and file use private permissions where supported,
updates are inter-process locked and atomically replaced, and stale sessions are
pruned on every update after 12 hours by default.

Override the path or stale-session duration with:

```sh
AURORA_CLAUDE_STATE_FILE=~/.local/state/aurora/claude-sessions.json
AURORA_CLAUDE_SESSION_TTL=12h
```

TTL values use Go duration syntax such as `30m`, `2h`, or `12h`. Invalid, zero, and
negative values fall back to 12 hours.

The relay defaults to `http://127.0.0.1:8080` and the source defaults to
`claude-code`. Override them with `AURORA_RELAY_URL` and `AURORA_SOURCE`.

### Build

From the repository root:

```sh
mkdir -p bin
go build -o bin/aurora-claude-hook ./cmd/aurora-claude-hook
```

### Configure Claude Code

Merge the following `hooks` entries into `~/.claude/settings.json`; preserve every
other existing setting and any existing hooks. Do not replace the whole file just
with this example. `/srv/dev/aurora` is the current development checkout path, not
a recommended production installation path.

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/srv/dev/aurora/bin/aurora-claude-hook",
            "async": true,
            "timeout": 3
          }
        ]
      }
    ],
    "Notification": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/srv/dev/aurora/bin/aurora-claude-hook",
            "async": true,
            "timeout": 3
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/srv/dev/aurora/bin/aurora-claude-hook",
            "async": true,
            "timeout": 3
          }
        ]
      }
    ],
    "StopFailure": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/srv/dev/aurora/bin/aurora-claude-hook",
            "async": true,
            "timeout": 3
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/srv/dev/aurora/bin/aurora-claude-hook",
            "async": true,
            "timeout": 3
          }
        ]
      }
    ]
  }
}
```

Restart Claude Code after editing the settings, then run `/hooks` to confirm that
the five user hooks are loaded and point to the absolute binary path above.

### Temporary raw hook capture

Raw capture is an opt-in diagnostic mode for inspecting the exact payloads emitted
by Claude Code. Raw payloads may contain sensitive information including prompts,
paths, session identifiers, and other Claude context. Enable it only intentionally,
use a capture directory protected from other users, and delete every captured file
after the experiment. Never enable `AURORA_CAPTURE_HOOKS` in production.

To capture into a private temporary directory, change only the `command` value in
the relevant existing hook entries, preserving all other user settings:

```json
"command": "AURORA_CAPTURE_HOOKS=/tmp/aurora-hooks /srv/dev/aurora/bin/aurora-claude-hook"
```

The hook creates the directory with private permissions where possible and creates
one non-overwriting `0600` JSON file per invocation. Capture is best effort and does
not change publication behavior when capture itself fails. Unset
`AURORA_CAPTURE_HOOKS` after diagnostics and securely delete the capture directory.
Raw capture remains separate from the session store and may retain all sensitive
fields present in the original hook payload.

### Manual end-to-end test

Start the local relay, publish a synthetic Claude lifecycle event, and read it back:

```sh
go run ./cmd/aurora-relay -listen 127.0.0.1:8080
```

In another terminal:

```sh
printf '%s\n' '{"hook_event_name":"UserPromptSubmit","session_id":"manual-test"}' | /srv/dev/aurora/bin/aurora-claude-hook
curl http://127.0.0.1:8080/presence
```

The response should contain `"source":"claude-code"` and `"state":"working"`.
