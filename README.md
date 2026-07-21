# Aurora

Aurora is an experimental physical status indicator for AI workers.

The project contains:

- `aurora-agent`: runs where Claude, Codex, or another supported AI tool runs.
- `aurora-relay`: receives presence snapshots over HTTP and exposes the latest snapshot.
- `device`: firmware for the Aurora ESP device.

Aurora must not transmit prompts, source code, file names, terminal output, or other work content.

## Aurora Relay

Aurora Relay is an in-memory HTTP service. Agents publish with `POST /presence`, and
clients such as the ESP read the current aggregate snapshot with `GET /presence`.

The relay retains the latest snapshot separately for each `source`. With only one
registered source, `GET /presence` returns that source's snapshot unchanged. With
multiple sources, the relay returns a synthetic snapshot with
`source="aurora-aggregate"` and aggregates states using this priority:

    error > attention > working > idle

A lower-priority update from one source therefore cannot hide a higher-priority
state from another source. The aggregate timestamp is the newest timestamp among
the sources currently contributing the winning state.

### Build and run locally

Build the relay from the repository root:

```sh
mkdir -p bin
go build -o bin/aurora-relay ./cmd/aurora-relay
```

For development that is only accessed from the same machine, retain the safe
loopback default:

```sh
go run ./cmd/aurora-relay
```

To serve the ESP over the local network, the relay must listen on all interfaces:

```sh
go run ./cmd/aurora-relay -listen 0.0.0.0:8080
```

The ESP uses Blue1's LAN address, for example
`http://192.168.0.247:8080/presence`; `localhost` on the ESP would refer to the ESP
itself. The relay API has no authentication, so binding to `0.0.0.0` exposes it to
the local network. Do not expose port 8080 to an untrusted network.

### Install as a systemd service on Blue1

The repository includes `deploy/systemd/aurora-relay.service` and a controlled
installer. The unit runs as `carl`, executes `/usr/local/bin/aurora-relay` without a
repository working directory, and explicitly listens on `0.0.0.0:8080`.

1. Inspect the unit and installer, then install the binary and unit:

```sh
./scripts/install-aurora-relay.sh
```

The installation builds the relay, installs the binary and unit, and runs
`systemctl daemon-reload`. It never enables, starts, stops, or restarts the service.
Existing installed files are not replaced unless `--replace` is supplied.

2. Stop the manually running relay before starting the systemd service. The
installer deliberately does not do this. If the manual relay still owns port 8080,
the systemd service cannot start.

3. Enable the installed service at boot:

```sh
sudo systemctl enable aurora-relay.service
```

4. Start the systemd service explicitly:

```sh
sudo systemctl start aurora-relay.service
```

5. Verify service state, journal logs, and the HTTP endpoint:

```sh
systemctl status aurora-relay.service
journalctl -u aurora-relay.service
curl -i http://127.0.0.1:8080/presence
```

A newly started relay has no in-memory snapshot. Until an agent publishes the first
status, `GET /presence` correctly returns `404 Not Found`.

For a reviewed upgrade, rerun the installer with `--replace`:

```sh
./scripts/install-aurora-relay.sh --replace
```

This installs the new binary and unit and runs `systemctl daemon-reload`, but does
not restart a running service. Explicitly restart it to begin using the new binary:

```sh
sudo systemctl restart aurora-relay.service
```

The installer may require `sudo`. Restart remains a separate operator action and is
never combined with installation.

Stop or disable the service with:

```sh
sudo systemctl stop aurora-relay.service
sudo systemctl disable aurora-relay.service
```

To uninstall it, stop and disable it first, then remove only its installed files and
reload systemd:

```sh
sudo rm /etc/systemd/system/aurora-relay.service
sudo rm /usr/local/bin/aurora-relay
sudo systemctl daemon-reload
```

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
| `PreToolUse` with `AskUserQuestion` | Set session to `attention` |
| `PostToolUse` with `AskUserQuestion` | Set session to `working` |
| `Notification` with `permission_prompt` | Set session to `attention` |
| `Notification` with `idle_prompt` | Set session to `idle` |
| Other `Notification` events | Set session to `attention` |
| `Stop` | Set session to `idle` |
| `StopFailure` | Set session to `error` |
| `SessionEnd` | Remove that session |

Observed Claude Code behavior is a `UserPromptSubmit` when work begins, followed by
`Stop` and an `idle_prompt` notification after a normal response. Both completion
events map to idle. Interactive questions are detected through `PreToolUse` and
`PostToolUse` events matched specifically to `AskUserQuestion`, producing the flow
`working -> attention -> working`. Permission prompts remain attention. Unknown
notification types also map defensively to attention. Session exit arrives as `SessionEnd`, commonly with a reason such as
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
    "PreToolUse": [
      {
        "matcher": "AskUserQuestion",
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
    "PostToolUse": [
      {
        "matcher": "AskUserQuestion",
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
the seven user hooks are loaded and point to the absolute binary path above.

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
