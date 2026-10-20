# Autobot v2 design

Status: proposed
Author: Jonathan Gabor
Date: 2026-10-20

## Summary

Autobot runs recurring personal chores on a Linux machine with an AI model and no supervision. A chore is a plain-language Markdown file. Autobot runs it on a schedule, keeps what it learned, and leaves the result where the user will see it.

This document proposes a redesign. The product stays the same. Three things change:

1. Each task declares what it may read and write, and the code enforces it.
2. Each task has its own schedule and its own memory.
3. Autobot becomes a small Go binary instead of a Bash script.

## Background: the current version (v1)

v1 is a single Bash script of about 750 lines, run by a user-level systemd unit.

- Tasks are numbered Markdown files (`1-review-notes.md`, `2-audit-user-services.md`) in `~/.config/autobot/`. They run in filename order. `0-system.md` holds the system prompt and is required.
- The model gets five tools: Read, Write, Edit, Grep and Glob. There is no shell and no network tool.
- After each run, a reflection step rewrites one shared `~/.local/state/autobot/state.md`. It must contain fixed sections and exactly three "next-run improvements".
- Scheduling uses `Restart=always` with `RestartSec=1h` on a simple service, so the script reruns an hour after each exit.
- The provider is any OpenAI-compatible chat completions endpoint.

Problems seen in v1:

- **Writes are not scoped.** Write and Edit accept any path the user can write, except the state file and system prompt. "Do not modify the originals" is only a request in the prompt.
- **Bugs fail silently.** The Grep tool reads a misnamed variable. Under `set -u` that part of the pipeline fails, and the tool reports "No matches found" on every call.
- **Every chore runs on one schedule.** All tasks run every hour. A weekly review and an hourly check cannot coexist.
- **One shared memory, rewritten whole.** Small models drop facts when they rewrite the full file. There is no history, so a bad reflection cannot be undone.
- **Onboarding is manual.** Users must write the system prompt from scratch and copy files into place. The environment file is only loaded by systemd, so a manual test run fails. A config error makes systemd retry an hour later without telling anyone.
- **One failed task stops the rest of the run.**
- **Results are hard to find.** Output goes to the journal or to files scattered across the user's folders.
- **JSON is handled in Bash.** Every tool call starts several `jq` processes, and quoting is fragile.

## Goals

- A new user can install, configure and see a first task succeed in under five minutes.
- A task can only touch the paths and capabilities it declares.
- Each task runs on its own schedule.
- What autobot learns is visible, bounded, versioned and reversible.
- Every run leaves a record that explains what it did and why.
- Local models remain first-class. Any OpenAI-compatible endpoint works.
- Install is one file with no runtime dependencies.

## Non-goals

- A web interface.
- A plugin system.
- Task dependency graphs.
- Multiple cooperating agents.
- Provider-specific SDKs.
- A long-running daemon.
- Running arbitrary shell commands.

Each of these can be revisited when a real task needs it.

## Design

### Overview

```
systemd timer ──▶ autobot run --due
                      │
                      ▼
                  scheduler ──▶ runner ──▶ agent loop ──▶ provider (OpenAI-compatible)
                                  │            │
                                  │            └──▶ tools ──▶ access checks
                                  │
                                  ├──▶ reflection ──▶ memory store (git)
                                  └──▶ run record ──▶ inbox, digest, notify
```

### Task format

A task is one Markdown file in `~/.config/autobot/tasks/`. A short header holds what the code must enforce. The body is the prompt.

```markdown
---
schedule: daily
read:  [~/Documents/Notes]
write: [~/Documents/Notes/next-actions.md]
---
Review my notes. Find unfinished tasks, repeated reminders and anything
time-sensitive. Write a prioritized action list.
```

Header fields:

| Field | Default | Meaning |
|---|---|---|
| `schedule` | `daily` | `hourly`, `daily`, `weekly` or a cron expression |
| `read` | none | Paths the task may read, list and search |
| `write` | none | Paths the task may write, beyond its own output folder |
| `network` | `false` | Allows the web search and fetch tools |
| `commands` | none | Allowlisted read-only commands, matched exactly |
| `model` | global | Overrides the model for this task |
| `max_rounds` | 20 | Cap on tool-call rounds |
| `enabled` | `true` | Turns the task off without deleting it |

The filename is the task's ID. There are no number prefixes. Due tasks run in alphabetical order. Each task runs separately, so one failure does not stop the others.

### Access enforcement

All file tools go through one access check:

1. Expand `~` and make the path absolute.
2. Resolve symlinks, including on the parent folder of a file that does not exist yet.
3. Allow the operation only if the resolved path is inside a declared root for that operation.

Defaults:

- A task can read nothing until it lists paths in `read`.
- A task can always write to its own output folder, and nowhere else unless listed in `write`.
- Autobot's own config, memory and run records cannot be written by tools.

Shell access does not exist. When a task needs system information, `commands` allows exact, read-only invocations such as `systemctl --user list-units --type=service`. They run without a shell, with a timeout and an output cap.

A later version may add Linux Landlock as a second layer at the operating-system level. The code-level checks stay in place either way.

### Tools

| Tool | Permission needed |
|---|---|
| `read` | `read` |
| `list` (glob) | `read` |
| `grep` | `read` |
| `write` | output folder or `write` |
| `edit` | `write` |
| `run_command` | `commands` |
| `web_search`, `web_fetch` | `network: true` |

Tools return clear errors when access is denied, so the model can change course instead of guessing.

### Outputs and the inbox

Each task's final answer is saved to `~/.local/state/autobot/inbox/<task>/<timestamp>.md`. Each run also writes a short digest that lists every task, its status and a link to its output.

Notifications are optional and set globally: `notify-send`, ntfy or email. The user should have one place to check, not a list of folders.

### Memory

Memory lives in `~/.local/state/autobot/memory/`:

- `shared.md` for facts useful to every task.
- `<task>.md` for each task.

After a task runs, reflection sees the task, its transcript summary and its current memory. It returns a list of changes, not a full rewrite:

```json
[
  {"op": "add", "section": "gotchas", "text": "Notes folder has drafts in _old/; skip them."},
  {"op": "remove", "id": "m-12"}
]
```

Autobot applies the changes, enforces a size limit per file and commits the state folder to git. `autobot memory diff` shows what changed, and `autobot memory undo` reverts the last reflection.

There is no fixed quota of improvements per run. Quotas push small models to invent filler.

### Scheduling

A systemd user timer runs `autobot run --due` every 15 minutes. Autobot stores each task's last run time in `~/.local/state/autobot/schedule.json`. It runs only tasks that are due.

- No process stays running between runs.
- A lock file prevents overlapping runs.
- A missed run (machine asleep) runs once on the next check. It does not catch up on every missed slot.
- `autobot init` prints the `loginctl enable-linger` command, because user services stop at logout without it.

### Run records

Each task run writes a folder in `~/.local/state/autobot/runs/<timestamp>-<task>/`:

- `transcript.jsonl` with every message and tool call.
- `report.md` with status, duration, tools used and the final answer.

Records older than a configurable age are pruned.

### Provider

One interface with one real implementation: an OpenAI-compatible chat completions client. Tests use a second implementation that returns scripted replies, which is the reason for the interface.

- `autobot check` sends one small request that requires a tool call. This confirms the endpoint, the key and tool-call support. Many small local models fail this, and users should learn it before a scheduled run does.
- Context limits are counted in tokens, using an estimate when the server does not report usage. Large tool outputs are cut, with a note telling the model what was cut.

### Configuration

```
~/.config/autobot/
├── config.toml      provider, model, defaults, notifications
├── system.md        optional; a built-in default is used when absent
└── tasks/
    └── review-notes.md

~/.local/state/autobot/
├── inbox/
├── memory/          git repository
├── runs/
├── schedule.json
└── autobot.lock
```

API keys come from environment variables or a `key_command` in `config.toml`, such as a password manager call. Keys are never written to run records.

## Command line

| Command | Purpose |
|---|---|
| `autobot init` | Create config, a default system prompt, one safe example task and the systemd timer. Never overwrites files. |
| `autobot check` | Test config, provider, key and tool-call support. List tasks with their next run time. |
| `autobot new <name>` | Create a task from a template. |
| `autobot run <task>` | Run one task now. |
| `autobot run <task> --dry-run` | Run without writing. Intended writes are shown as diffs. |
| `autobot run --due` | Run every task that is due. Used by the timer. |
| `autobot log` | List recent runs with status. |
| `autobot show <run>` | Show one run's report and transcript. |
| `autobot memory diff` / `undo` | See or revert what reflection changed. |

Errors name the next step, for example "no tasks found in ~/.config/autobot/tasks (run `autobot new <name>`)".

The quick start in the README is:

```bash
go install example.invalid/autobot@latest   # or download the release binary
autobot init
$EDITOR ~/.config/autobot/config.toml
autobot check
autobot run review-notes
```

## Implementation

- Language: Go. One static binary, standard library first.
- Packages: `task` (parse and validate), `access` (path checks), `tools`, `provider`, `agent` (the loop), `memory`, `schedule`, `record`, `cli`.
- The agent loop is the only place that talks to the provider. Tools never call the provider.

### Testing

- **Access checks:** a table of escape attempts. Covers `..`, symlinks inside and outside roots, `~` handling, a new file under a symlinked folder, and case and trailing-slash variants.
- **Agent loop:** replayed conversations through the fake provider. Covers tool errors, round limits, malformed tool calls and missing content.
- **Reflection:** invalid change lists are rejected and memory is left untouched.
- **Scheduler:** due calculation across missed runs, clock changes and new tasks.
- **CLI:** `init` on an empty home and on an existing setup, using a temporary `HOME`.

## Alternatives considered

### Keep improving the Bash script

Most of the user value comes from enforced access and per-task schedules. Both could be added to the Bash version.

- **For:** one readable file, no build step, easy to audit in one sitting.
- **Against:** correct path checks (symlinks, new files under linked folders) are hard to get right and to test in Bash. JSON handling through `jq` is slow and fragile. The v1 Grep bug shows how errors hide.

Decision: rewrite in Go before adding access control, not after. If the rewrite is delayed, fix the Grep bug and add `init`, `check` and single-task runs to v1 first.

### Keep one shared memory file

Simpler, but every task pays for every other task's memory in context. Unrelated facts also distract small models. Per-task memory with a small shared file keeps context relevant.

### Run as a daemon

A daemon could schedule precisely and watch files. It adds a process to supervise and a failure mode to debug. A timer that wakes every 15 minutes is precise enough for chores.

## Migration from v1

`autobot migrate` reads a v1 config folder and:

1. Moves `N-name.md` to `tasks/name.md` with `schedule: hourly`. That matches v1 behavior.
2. Adds `read` and `write` headers guessed from paths named in the task text, marked with a `# review` comment. The command prints each guess.
3. Uses `0-system.md` as `system.md`.
4. Copies v1 `state.md` into `memory/shared.md`.
5. Prints the commands to replace the old service with the new timer.

Tasks with unreviewed headers are disabled until the user confirms them. Guessed permissions must not grant access silently.

## Open questions

1. Should due tasks run in parallel? Local models usually serve one request at a time, so sequential is the safer default.
2. Is a 15 minute timer the right granularity, or should `init` write a timer that matches the shortest task schedule?
3. Should `write` allow whole folders, or only named files plus the output folder?
4. Which web search provider for `network` tasks? v1 planned Kagi.
5. How long to keep run records by default: 30 days, or a size cap?
6. Should reflection run for failed tasks? Failures teach the most, but a broken provider would produce noise.
