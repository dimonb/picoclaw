# Scheduled Tasks and Cron Jobs

> Back to [README](../README.md)

PicoClaw stores scheduled jobs in the current workspace and can run them either as reminders, full agent turns, or shell commands.

## Schedule Types

PicoClaw currently uses three schedule forms in the cron tool:

- `at_seconds`: one-time job, relative to now. After it runs, the job is removed from the store.
- `every_seconds`: recurring interval, in seconds.
- `cron_expr`: recurring cron expression such as `0 9 * * *`.

The CLI command `picoclaw cron add` currently supports recurring jobs only:

- `--every <seconds>`
- `--cron '<expr>'`

There is no CLI flag for a one-time `at` job today.

Examples:

```bash
picoclaw cron add --name "Daily summary" --message "Summarize today's logs" --cron "0 18 * * *"
picoclaw cron add --name "Ping" --message "heartbeat" --every 300
```

## Agent Tool Actions

The agent-facing `cron` tool supports these actions:

- `add`: create a new job.
- `list`: show accessible job names, ids, and schedules.
- `get`: fetch one accessible persisted job by `job_id`, including its saved payload.
- `update`: partially update one accessible job by `job_id`; omitted fields are preserved.
- `remove`, `enable`, `disable`: existing management actions.

When rescheduling an existing task, use `list -> get -> update`. Do not use
`remove -> add` just to change the schedule, because recreating a job can drop
the original prompt, delivery target, or command payload.

Remote channel access is scoped to the current `channel/chat_id`: remote callers
can only list, get, or update jobs whose saved `payload.channel` and `payload.to`
match the current conversation. When both the job and the caller know their
session, the session keys must match too — one chat can host several sessions
(forum topics, per-sender scopes). Command jobs include a shell command payload, so
they can only be listed, inspected, or updated from internal channels or remote
channels allowed by `tools.cron.command_allowed_remotes`.

Example tool calls:

```json
{"action":"get","job_id":"79095b2f5685a0f2"}
```

```json
{"action":"update","job_id":"79095b2f5685a0f2","cron_expr":"30 10 * * *"}
```

`update` accepts `name`, `message`, `command`, `session`, and exactly one
schedule field (`at_seconds`, `every_seconds`, or `cron_expr`).
Omit `command` to preserve it, set `command` to a non-empty string to replace
it, or set `command` to `""` to clear it. Command updates require the same
channel allowlist and confirmation gates as command creation.

## Where a Firing Runs

A firing is not a standalone task: it is injected back into the session that
scheduled the job, as an inbound message tagged `[cron]`. The agent therefore
sees the trigger with the history, summaries, and skills of the conversation
that created it, and can tell a firing apart from a real user message.

To make that possible, `add` records the scheduling turn alongside the schedule:

- `payload.sessionKey` — the session to inject into
- `payload.agentId` — the agent that owned the scheduling turn
- `payload.origin` — the rest of the inbound context (chat type, topic, space,
  account, sender), because the session key is derived from the whole scope and
  cannot be rebuilt from `channel` + `chat_id` alone

Because the firing travels through the normal inbound path, it is serialized
with live turns in the same session: if a turn is already running, the trigger is
queued as a steering message instead of racing it.

Jobs created before these fields existed — and jobs created by
`picoclaw cron add`, which has no session — carry no session key. They are
routed to the channel's natural session instead.

### `session: origin` (default)

Inject the firing into the session that scheduled the job.

### `session: isolated`

Run every firing in a fresh, empty session. Useful only for standalone monitors
whose output should never touch a conversation's context. A job scheduled from
inside an isolated firing does not inherit that throwaway session.

Set the default with `tools.cron.session_mode`; a per-job `session` argument
wins over it.

## Execution Modes

Jobs are stored with a message payload and execute in two modes:

### Message jobs

The saved message is injected into the session as a cron trigger. Use this for
scheduled work that may need reasoning, tools, or a generated reply.

### `command`

When a job includes `command`, PicoClaw runs that shell command through the
`exec` tool. The saved `message` becomes descriptive text only; the scheduled
action is the shell command.

How the result is delivered is controlled by `tools.cron.command_delivery`:

- `session` (default): the command's exit state and output are injected into the
  session as a cron trigger, and the agent decides whether the result is worth
  reporting. Output is truncated so a chatty script cannot flood the context.
- `raw`: the command output is published straight to the chat without agent
  processing.

The current CLI `picoclaw cron add` command does not expose a `command` flag.

## Config and Security Gates

### `tools.cron`

`tools.cron.enabled` controls whether the agent-facing `cron` tool is registered. Default: `true`.

If you disable `tools.cron`, users can no longer create or manage jobs through the agent tool. The gateway still starts `CronService`, but it does not install the job execution callback. As a result, due jobs do not actually run; one-time jobs may be deleted and recurring jobs may be rescheduled without executing their payload. The CLI still uses the same job store.

`tools.cron.exec_timeout_minutes` sets the timeout used for scheduled command execution. Default: `5`. Set `0` for no timeout.

`tools.cron.session_mode` sets the default session a firing runs in: `origin`
(default) or `isolated`. See [Where a Firing Runs](#where-a-firing-runs).

`tools.cron.command_delivery` sets how scheduled command output reaches the
user: `session` (default) or `raw`. See [`command`](#command).

### `tools.exec`

Scheduled command jobs depend on `tools.exec.enabled`. Default: `true`.

If `tools.exec.enabled` is `false`:

- new command jobs are rejected by the cron tool
- existing command jobs publish a `command execution is disabled` error when they fire

`tools.exec.allow_remote` is still enforced by the exec tool, but cron command scheduling has its own channel gate when the job is created. In practice, reminder jobs can be scheduled from remote channels, while scheduled command jobs are limited to internal channels and configured remote channels.

### `allow_command`

`tools.cron.allow_command` defaults to `true`.

This is not a hard disable switch. If you set `allow_command` to `false`, PicoClaw still allows a command job when the caller explicitly passes `command_confirm: true`.

Command jobs also require either an internal channel or a remote channel allowed by `tools.cron.command_allowed_remotes`. Non-command reminders do not have that restriction.

### `command_allowed_remotes`

`tools.cron.command_allowed_remotes` defaults to an empty list. With the default empty list, remote channels cannot schedule command jobs.

Entries can be either a channel name or a channel plus chat id:

- `telegram` allows command jobs from any Telegram chat.
- `telegram:1234567890` allows command jobs only from that exact Telegram chat id.
- `*` allows command jobs from every non-empty channel.

Warning: `*` is potentially dangerous because any remote channel that can talk
to PicoClaw can schedule shell commands. Use it only when every enabled remote
channel and chat is trusted to request command execution.

This setting only controls the remote-channel gate. It does not bypass `tools.cron.allow_command`, `command_confirm`, `tools.exec.enabled`, or the exec tool's command safety checks.

Example:

```json
{
  "tools": {
    "cron": {
      "enabled": true,
      "exec_timeout_minutes": 5,
      "session_mode": "origin",
      "command_delivery": "session",
      "allow_command": true,
      "command_allowed_remotes": [
        "telegram:1234567890"
      ]
    },
    "exec": {
      "enabled": true
    }
  }
}
```

## Persistence and Location

Cron jobs are stored in:

```text
<workspace>/cron/jobs.json
```

By default, the workspace is:

```text
~/.picoclaw/workspace
```

If `PICOCLAW_HOME` is set, the default workspace becomes:

```text
$PICOCLAW_HOME/workspace
```

Both the gateway and `picoclaw cron` CLI subcommands use the same `cron/jobs.json` file.

Notes:

- one-time `at_seconds` jobs are deleted after they run
- recurring jobs stay in the store until removed
- disabled jobs stay in the store and still appear in `picoclaw cron list`
