# Branch Structure and Upstream Sync Workflow

This repository is a fork of [sipeed/picoclaw](https://github.com/sipeed/picoclaw).

## Remotes

| Remote   | URL                                          | Purpose             |
|----------|----------------------------------------------|---------------------|
| `origin` | github.com:dimonb/picoclaw.git               | fork (this repo)    |
| `upstream` | github.com:sipeed/picoclaw.git             | upstream source     |

## Branches

### `main`
Tracks `upstream/main` exactly — no fork-local commits.
When upstream moves, reset `main` hard to `upstream/main` and force-push.

### `dev`
The integration branch containing all local features on top of upstream.
This is the working branch — build, test, and run from here.
`AGENTS.md` lives here (not on `main`).

### `pr/0X-*` — Feature Branches

| Branch                        | Tip commit | Status              | Feature group                                                |
|-------------------------------|------------|---------------------|--------------------------------------------------------------|
| `pr/01-message-history`       | `0cda85b`  | ✅ done              | Store sender first/last name and message author in session   |
| `pr/01.01-message-tool`       | `5d1b7b8`  | ✅ done              | Explicit message send/edit tool, reaction support, meta JSON |
| `pr/01.02-media`              | `c5a249b`  | ✅ done              | Organized media storage under workspace/media/YYYYMMDD/      |
| `pr/01.03-history-compaction` | `99a4a5a`  | ✅ done              | Thread-aware compaction, journal, two-pass summarisation     |
| `pr/01.04-cron`               | `bf0819e`  | ✅ done              | Cron delivery modes, session binding, startup recovery       |
| `pr/02-concurrency`           | `2527e1e`  | ✅ done              | Per-session concurrency via sessionDispatcher                |
| `pr/03-providers`             | `3d49145`  | ✅ done              | OpenAI responses API, correct fallback provider routing      |
| `pr/04-link-fix`              | `c36b06a`  | ✅ merged upstream   | Fix Telegram HTML parser corrupting links with underscores   |
| `pr/05-group-allow-filter`    | `05df147`  | ✅ done              | Telegram allow_chats filter                                  |
| `pr/07-tasktool`              | —          | ⬜ deferred          | TaskTool / channels.Manager DI / outbound plumbing           |

### Archived branches (on origin)

| Branch                              | Superseded by                  |
|-------------------------------------|--------------------------------|
| `archive/pr/02-compaction`          | `pr/01.03-history-compaction`  |
| `archive/pr/03-telegram`            | `pr/01.01-message-tool`        |
| `archive/pr/04-cron`                | `pr/01.04-cron`                |
| `archive/pr/05-concurrency`         | `pr/02-concurrency`            |
| `archive/pr/06-media`               | `pr/01.02-media`               |
| `archive/pr/08-reaction`            | `pr/01.01-message-tool`        |
| `archive/pr/09-providers`           | `pr/03-providers`              |

### Legacy marker branches (`local/0X-*`)
Stale sha markers from the old layout. Kept for reference, do not use.

### `backup/main-2026-03-17`
Snapshot of `main` taken on 2026-03-17 before the reorganisation. Do not delete.

---

## Syncing Upstream Changes into `main` and `dev`

Run this whenever `upstream/main` has moved forward:

```bash
# 1. Fetch upstream
git fetch upstream

# 2. Reset main to upstream/main
git checkout main
git reset --hard upstream/main
git push --force-with-lease origin main

# 3. Merge refreshed main into dev (resolve conflicts once, then commit)
git checkout dev
git merge main

# 4. Run tests
go test ./...

# 5. Push
git push origin dev
```

### Conflict resolution hints

| File                        | Our change                                  | Strategy                                |
|-----------------------------|---------------------------------------------|-----------------------------------------|
| `pkg/gateway/gateway.go`    | `PersistentFileMediaStoreWithCleanup`       | Keep ours, adopt upstream variable names|
| `pkg/tools/cron.go`         | `allowRemote`, mode-based dispatch          | Merge both feature sets                 |
| `pkg/tools/cron_test.go`    | Additional mode/session tests               | Keep both test suites                   |
| `pkg/agent/loop.go`         | `agentResponse`, `OnDelivered` callback     | Keep ours, pull in upstream additions   |
| `pkg/agent/context.go`      | Extended context keys                       | Keep ours, pull in upstream additions   |

### After a large upstream merge into `dev`

If a file has deep conflicts, compare with `git diff upstream/main...HEAD -- <file>`
and resolve by applying our semantic change on top of the new upstream version.

---

## Local-only files (not in upstream)

- `pkg/agent/summarization_prompts.go` — thread-aware compaction prompts
- `pkg/media/store.go` — `PersistentFileMediaStore` with cleanup
- `pkg/tools/reaction.go` — reaction tool
- `AGENTS.md` — this file; lives on `dev`, not on `main`
