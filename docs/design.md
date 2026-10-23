# Autobot v2 design

Status: implemented on the `design/autobot-v2` branch
Author: Jonathan Gabor
Date: 2026-10-20

## Summary

Autobot runs recurring personal chores on a Linux machine with an AI model and no supervision. A chore is a plain-language Markdown file. Autobot runs it on a schedule, keeps what it learned, and leaves the result where the user will see it.

This document describes the redesign. The product stays the same. Three things change:

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

The header is optional, and a header with only comments counts as empty. Blank lines before the opening `---` and trailing spaces on the `---` lines are allowed. Unknown fields are errors, so a typo such as `shedule:` is reported instead of ignored. Paths in `read` and `write` must start with `/` or `~/`. A path can name a folder, which covers everything inside it, or a single file, which covers only itself. A path is a folder if it exists as one when the task runs; a folder that does not exist yet is written with a trailing `/`, such as `~/reports/`. Missing folders are only created inside the output folder or a folder root, never above a declared folder, so a file root can never turn into a folder and a folder root cannot be written as a file. In YAML a bare `~` means null, so autobot reads `read: [~]` as the home folder rather than dropping it.

The filename is the task's ID. There are no number prefixes. Names use lowercase letters, digits, `-` and `_`. `shared` (shared memory) and `digests` (the digest folder) are reserved. Due tasks run in alphabetical order. Each task runs separately, so one failure does not stop the others.

A task file that fails to parse is reported in the digest, once per distinct error, and the other tasks still run.

### Access enforcement

All file tools go through one access check:

1. Expand `~` and make the path absolute. Relative paths resolve against the task's output folder.
2. Resolve symlinks, including on the parent folder of a file that does not exist yet.
3. Allow the operation only if the resolved path is inside a declared root for that operation. Roots are resolved the same way, and comparison is by whole path components, so `~/notes` does not cover `~/notes-old`.

Defaults:

- A task can read nothing until it lists paths in `read`.
- A task can always read and write its own output folder, and write nowhere else unless listed in `write`.
- Autobot's own config folder and state folder (memory, run records, schedule) cannot be read or written by tools, even when a task lists `~`. The task's output folder inside the state folder is the one exception. Files that symlinks inside those folders point to are protected too, so a config folder managed by a dotfile tool stays off limits. `/proc` is protected as well, because it exposes autobot's own environment, including API keys.

Shell access does not exist. When a task needs system information, `commands` allows exact, read-only invocations such as `systemctl --user list-units --type=service`. They run without a shell, in an empty temporary folder, with a minimal environment, a timeout and an output cap. The timeout kills the command's whole process group. Shell syntax outside quotes, such as pipes, redirects and `$`, is rejected when the task is loaded.

Autobot cannot verify that a listed command is read-only; that is the task author's promise. Listing `sh -c '...'` hands the model a shell. Commands that run code from files the task can write, such as `git` or `make` in a folder listed under `write`, do the same.

File tools refuse pipes, devices and other files that are not regular files. No file can be written or edited beyond 20 MB. Folder walks in `list` and `grep` stop after one minute, after 200,000 entries, or when the run is cancelled, and say that the results are incomplete. `grep` refuses regular expressions that compile to very large programs and skips lines over 64 KB, so one call cannot run for hours. A tool that hits a bug returns an error to the model instead of stopping the run.

`web_fetch` refuses loopback, private, link-local and similar addresses after DNS resolution, so a task with network access cannot reach services on the machine or the local network.

A later version may add Linux Landlock as a second layer at the operating-system level. The code-level checks stay in place either way.

### Tools

| Tool | Works on |
|---|---|
| `read`, `list` (glob), `grep` | the output folder and `read` paths |
| `write` | the output folder and `write` paths |
| `edit` | paths that are both readable and writable |
| `run_command` | only offered with `commands` |
| `web_search`, `web_fetch` | only offered with `network: true` |

The file tools are always offered, since every task has its output folder. `run_command` and the web tools are not offered at all to a task without the permission. Tools return clear errors when access is denied, naming the path, the allowed paths and the header field to change, so the model can change course instead of guessing. `edit` requires its text to appear exactly once unless `replace_all` is set. It needs read access too, because its answer would reveal whether text is in the file. `list` supports `**`; it does not support `{a,b}` and says so.

### Outputs and the inbox

Each task's final answer is saved to `~/.local/state/autobot/inbox/<task>/<timestamp>.md`. Each run also writes a short digest to `inbox/digests/<timestamp>.md` that lists every task, its status and a link to its output. The latest digest is copied to `inbox/latest.md`.

When the configuration itself is broken, a timer run writes a digest with the error, so the failure shows up in the inbox instead of only in the journal.

Notifications are optional and set globally: `notify-send`, ntfy or email. `notify.on` is `always` or `failure`. Every notification has a deadline, so a stalled mail server cannot hold up the run. The user should have one place to check, not a list of folders.

### Memory

Memory lives in `~/.local/state/autobot/memory/`:

- `shared.md` for facts useful to every task. Item IDs start with `s-`.
- `<task>.md` for each task. Item IDs start with `m-`.

Each file is Markdown with five sections: Locations, Procedures, Gotchas, Insights and Follow-ups. Each item is one bullet with an ID, such as `- [m-3] Drafts are in _old/; skip them.` People can edit the files by hand. Lines without an ID get one, and other text is kept as items rather than dropped.

Both files are part of the task's system prompt. After a task runs, reflection sees the task, its outcome, a summary of its tool calls and its current memory. It returns a list of changes, not a full rewrite:

```json
[
  {"op": "add", "scope": "task", "section": "Gotchas", "text": "Notes folder has drafts in _old/; skip them."},
  {"op": "remove", "id": "m-12"}
]
```

Autobot validates every change before applying any. An invalid list, or one that would push a file over the size limit (`memory.max_bytes`, 8000 bytes by default), is sent back to the model once with the error. The limit applies only to files the list changes, and a file that is already over the limit, for example after a large import, may still shrink. A list that leaves both files as they were is recorded as "no memory changes" and makes no commit. If the second answer also fails, memory is left untouched and the run record says why.

The memory folder is a git repository, written with a pure Go git library, so no `git` binary is needed. Every accepted reflection is one commit. Hand edits are committed separately at the start of each task run, so undoing a reflection never undoes them. `autobot memory diff` shows what changed, and `autobot memory undo` reverts the last change. Running undo again steps one more change back. Undo refuses to discard uncommitted hand edits unless given `--force`, and refuses to revert a commit of hand edits.

Reflection runs after failed tasks too, since failures teach the most. It is skipped when the provider itself failed, because there is nothing to learn and the request would fail again.

There is no fixed quota of improvements per run. Quotas push small models to invent filler. Secrets known to autobot are removed from memory items before they are stored.

Tasks with `network: true` neither see nor change shared memory. Otherwise a fact one task read could leak out through another task's network access, and text from a web page could steer tasks that have more access.

### Scheduling

A systemd user timer runs `autobot run --due` every 15 minutes. Autobot stores each task's last run time and status in `~/.local/state/autobot/schedule.json`. It runs only tasks that are due.

- A task that has never run is due on the next check.
- A task is due when its next scheduled time is at most two minutes away. Without that grace, a daily task started a few seconds after a timer tick would slip by one tick every day. A cron slot that ran early because of the grace counts as done.
- The last run time is the start of the check, not the end of the task, so long tasks do not push later ones back. It is saved before the task starts, so a task that crashes autobot is not started again at every check. A crash inside one task is reported as that task's failure, and the tasks after it still run.
- A cron expression that never matches a date, such as `0 0 30 2 *`, is rejected when the task is loaded.
- A missed run (machine asleep) runs once on the next check. It does not catch up on every missed slot.
- If the last run time is in the future, the clock moved back, and the task is due rather than stuck.
- Running a task by name also counts as its last run. A dry run does not.
- When the provider or its key fails before any tool ran, the task did not really run. Its last run time stays, so the next check retries it, and the same failure is reported once rather than at every check. A provider failure after tools ran counts as a run, because the task may already have changed files.
- No process stays running between runs.
- A lock file prevents overlapping runs.
- `autobot init` prints the `loginctl enable-linger` command, because user services stop at logout without it. `autobot check` warns when lingering is off.
- The service unit sets `TimeoutStartSec=3h`, because a oneshot unit has no start timeout by default and a hung run would hold the lock forever.

### Run records

Each task run writes a folder in `~/.local/state/autobot/runs/<timestamp>-<task>/`:

- `transcript.jsonl` with every message and tool call.
- `report.md` with status, duration, model, tools used, tokens, files written, the reflection outcome and the final answer.
- `meta.json` with the same summary in a form `autobot log` reads.

Every attempt leaves a record, including runs that fail before the model is called, such as a failing `key_command`. A run that was killed shows as incomplete. Records older than `records.keep_days` (30 by default) are pruned after each run.

### Provider

One interface with one real implementation: an OpenAI-compatible chat completions client. Tests use a second implementation that returns scripted replies, which is the reason for the interface.

- The client accepts the variations local servers send: `null` content, content as a list of parts, and tool arguments as an object instead of a string.
- Rate limits, server errors and network errors are retried twice with backoff. Other errors are not retried.
- `autobot check` sends one small request that offers a test tool and asks the model to call it. This confirms the endpoint, the key and tool-call support. Many small local models fail this, and users should learn it before a scheduled run does.
- Context limits are counted in tokens, estimated from text length and calibrated against the token counts the server reports. When the conversation nears the limit, the oldest tool outputs are replaced with a note. A single tool output longer than `defaults.max_tool_output_tokens` is cut, with a note telling the model what was cut and how to ask for less.
- `<think>` blocks from reasoning models are removed from final answers.

### Configuration

```
~/.config/autobot/
├── config.toml      provider, model, defaults, notifications, search
├── environment      optional KEY=VALUE lines, such as API keys
├── system.md        optional; a built-in default is used when absent
└── tasks/
    └── hello.md

~/.local/state/autobot/
├── inbox/
│   ├── <task>/
│   ├── digests/
│   └── latest.md
├── memory/          git repository
├── runs/
├── schedule.json
└── autobot.lock
```

Settings left out of `config.toml` use defaults. Unknown settings are errors.

API keys come from the environment variable named by `provider.api_key_env` (`OPENAI_API_KEY` by default) or from `provider.key_command`, such as a password manager call. Autobot loads `~/.config/autobot/environment` itself, and the systemd unit reads the same file, so a manual run and a timer run see the same keys. For a manual run, a variable already set in the shell wins, which allows a one-off override. Keys are removed from run records, inbox outputs and memory.

Autobot always gives the model the current date and time, the home folder, the output folder and its access, so tasks do not need to say them.

## Command line

| Command | Purpose |
|---|---|
| `autobot init` | Create config, a default system prompt, one safe example task and the systemd units. Never overwrites files. |
| `autobot check [--offline]` | Test config, provider, key and tool-call support. List tasks with their next run time. `--offline` skips the provider request. |
| `autobot new <name>` | Create a task from a template. The new task is disabled until its author sets `enabled: true`, so the timer does not run a half-written task. |
| `autobot run <task>...` | Run tasks now. |
| `autobot run <task> --dry-run` | Run without writing. Intended writes are shown as diffs. No memory change, record or schedule update. |
| `autobot run --due` | Run every task that is due. Used by the timer. |
| `autobot log [-n N] [task]` | List recent runs with status. |
| `autobot show [run] [--full]` | Show one run's report and transcript. The run can be its ID, a unique ID prefix or a task name for its latest run. |
| `autobot memory diff [-n N]` / `undo [--force]` | See or revert what reflection changed. |
| `autobot migrate [--from DIR]` | Convert a v1 setup. |
| `autobot version` | Print the version. |

Errors name the next step, for example "no task named "notes" in ~/.config/autobot/tasks (see `autobot check`, or create it with `autobot new notes`)". Exit codes are 0 for success, 1 for failures and 2 for usage errors.

The quick start in the README is:

```bash
CGO_ENABLED=0 go install github.com/jgabor/autobot/cmd/autobot@latest   # or download the release binary
autobot init
$EDITOR ~/.config/autobot/config.toml
autobot check
autobot run hello
```

## Implementation

- Language: Go. One static binary (built with `CGO_ENABLED=0`), standard library first.
- Dependencies: a TOML parser (BurntSushi/toml), a YAML parser for task headers (yaml.v3), a cron parser (robfig/cron), a pure Go git library (go-git), a unified diff writer (go-difflib) and the HTML tokenizer from golang.org/x/net.
- Packages: `paths`, `config`, `task` (parse and validate), `schedule`, `lock`, `access` (path checks), `tools`, `provider`, `agent` (the loop), `memory`, `reflection`, `record`, `inbox`, `notify`, `migrate`, `app` (wires the parts together for each command) and `cli`.
- The agent loop is the only place that talks to the provider during a task. Tools never call the provider. Reflection makes its own request after the loop ends.

### Testing

- **Access checks:** a table of escape attempts. Covers `..`, symlinks inside and outside roots, `~` handling, a new file under a symlinked folder, protected autobot files under a readable home, shared prefixes, and case and trailing-slash variants.
- **Agent loop:** conversations through the fake provider. Covers tool errors, round limits, malformed tool calls, missing IDs, empty replies, cut outputs and context limits.
- **Reflection:** invalid change lists are rejected and memory is left untouched, with one retry. Valid lists are committed and can be diffed and undone.
- **Scheduler:** due calculation across missed runs, clock changes, timer drift and new tasks.
- **Tools:** every tool, including dry-run diffs, command allowlists and timeouts, refused local addresses and search through a test server.
- **Provider:** the wire format against a test server, retries and error handling.
- **CLI:** `init` on an empty home and on an existing setup, using a temporary `HOME`. A full run through a test server: tool calls, writes, inbox, digest, run record, schedule, memory, `log`, `show`, `memory diff` and `memory undo`. Dry runs, due selection, failures that do not stop other tasks, notifications, redaction, `check` and `migrate`.

## Alternatives considered

### Keep improving the Bash script

Most of the user value comes from enforced access and per-task schedules. Both could be added to the Bash version.

- **For:** one readable file, no build step, easy to audit in one sitting.
- **Against:** correct path checks (symlinks, new files under linked folders) are hard to get right and to test in Bash. JSON handling through `jq` is slow and fragile. The v1 Grep bug shows how errors hide.

Decision: rewrite in Go before adding access control, not after.

### Keep one shared memory file

Simpler, but every task pays for every other task's memory in context. Unrelated facts also distract small models. Per-task memory with a small shared file keeps context relevant.

### Run as a daemon

A daemon could schedule precisely and watch files. It adds a process to supervise and a failure mode to debug. A timer that wakes every 15 minutes is precise enough for chores.

## Migration from v1

`autobot migrate` reads a v1 config folder and:

1. Moves `N-name.md` to `tasks/name.md` with `schedule: hourly`. That matches v1 behavior.
2. Adds `read` and `write` headers guessed from paths named in the task text, marked with a `# review` comment. A path in a sentence about writing or saving is a write path; any other path is a read path, cut back to the folder before its first glob character. The command prints each guess.
3. Uses `0-system.md` as `system.md`.
4. Writes `config.toml` from the v1 `AUTOBOT_*` settings in the environment or the `environment` file, looking first in the v1 folder. API keys stay in the `environment` file.
5. Copies v1 `state.md` into `memory/shared.md`, mapping its sections to the new ones, and commits it.
6. Prints the commands to replace the old service with the new timer.

Tasks with unreviewed headers are disabled until the user confirms them. Guessed permissions must not grant access silently. Migration never overwrites a v2 file, and running it twice changes nothing.

## Decisions on earlier open questions

1. **Parallel tasks:** no. Due tasks run one after another, because local models usually serve one request at a time.
2. **Timer granularity:** a fixed 15 minute timer. Finer cron schedules run at the next check.
3. **Write scope:** `write` accepts folders and single files. Single files are the safer habit, and the template shows one.
4. **Web search:** Kagi, with the key in `KAGI_API_KEY` by default. `web_fetch` needs no key.
5. **Record retention:** 30 days, configurable with `records.keep_days`.
6. **Reflection after failures:** yes, except when the provider failed.
