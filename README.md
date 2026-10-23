# autobot

autobot runs recurring chores on your Linux machine with an AI model while nobody is watching. Each chore is a Markdown file with a paragraph of instructions and a short header. autobot runs it on a schedule, keeps what it learned for next time, and leaves the result in one inbox.

- Each task lists the paths it may read and write, and the code checks every file operation against those lists.
- Each task has its own schedule: hourly, daily, weekly or a cron expression.
- What autobot learns goes in a small Markdown file per task, versioned in git.
- Any OpenAI-compatible endpoint works, including local servers such as Ollama, llama.cpp and vLLM.
- It is one binary with no runtime dependencies, started by a systemd timer rather than a daemon.

The design is in [docs/design.md](docs/design.md).

## Quick start

```bash
CGO_ENABLED=0 go install github.com/jgabor/autobot/cmd/autobot@latest
autobot init
$EDITOR ~/.config/autobot/config.toml     # set provider.url and provider.model
autobot check                             # tests the provider and tool calling
autobot run hello                         # runs the example task
```

Then turn on the schedule:

```bash
systemctl --user daemon-reload
systemctl --user enable --now autobot.timer
loginctl enable-linger "$USER"            # keep running after you log out
```

The model must support tool calling. `autobot check` tells you if it does not.

If your provider needs an API key, put it in `~/.config/autobot/environment`:

```bash
echo 'OPENAI_API_KEY=sk-...' >> ~/.config/autobot/environment
chmod 600 ~/.config/autobot/environment
```

Both manual runs and the timer read that file. You can use `provider.api_key_command` in `config.toml` instead, for example with a password manager.

## Writing a task

```bash
autobot new review-notes
$EDITOR ~/.config/autobot/tasks/review-notes.md
autobot run review-notes --dry-run       # shows intended writes as diffs, writes nothing
```

New tasks start with `enabled: false`. Set it to `true` when the task is ready for the timer.

A task file looks like this:

```markdown
---
schedule: daily
read:
  - ~/Documents/Notes
write:
  - ~/Documents/Notes/next-actions.md
---
Review my notes in ~/Documents/Notes.

List the Markdown files there and read up to five of them. If the folder does not
exist, say so and stop. Find unfinished tasks and anything time-sensitive.

Write a prioritized action list to ~/Documents/Notes/next-actions.md.
Do not change the original note files.
```

Header fields:

| Field | Default | Meaning |
|---|---|---|
| `schedule` | `daily` | `hourly`, `daily`, `weekly`, or a cron expression such as `"0 9 * * 1"` |
| `read` | none | Folders or files the task may read, list and search. End a folder that does not exist yet with `/` |
| `write` | none | Folders or files the task may write, beyond its output folder |
| `network` | `false` | Allow `web_search` and `web_fetch` |
| `commands` | none | Exact, read-only commands the task may run, such as `systemctl --user list-units --type=service` |
| `model` | from config | Use a different model for this task |
| `max_rounds` | 20 | Maximum tool-call rounds |
| `enabled` | `true` | Set to `false` to pause the task |

Tasks work best when they:

- name the exact folders or files to look at,
- limit how much to read, such as "read up to ten files",
- say what to do if a folder is missing,
- say what the answer should contain, and where to write a file if the inbox is not enough,
- say what must not be changed.

Task names use lowercase letters, digits, `-` and `_`; `shared` and `digests` are reserved. Every task can read and write its own output folder, `~/.local/state/autobot/inbox/<task>/`, without listing it. Relative paths point there. autobot's own config and state folders are always off limits to tasks.

More examples are in [examples/tasks](examples/tasks).

### Tools the model gets

| Tool | Needs |
|---|---|
| `read`, `list`, `grep` | a `read` path, or the output folder |
| `write` | a `write` path, or the output folder |
| `edit` | a path that is both readable and writable |
| `run_command` | `commands` |
| `web_search`, `web_fetch` | `network: true` (search uses Kagi; set `KAGI_API_KEY`) |

There is no shell. Allowed commands run in an empty temporary folder with a timeout. `web_fetch` refuses local and private addresses. Tasks with `network: true` do not see or change shared memory.

Do not list commands that give the model a shell, such as `sh -c '...'`, or that run code from files the task can write, such as `git` or `make` in a folder listed under `write`.

## Where results go

- `~/.local/state/autobot/inbox/latest.md` is the digest of the last run: every task, its status and a link to its output.
- `~/.local/state/autobot/inbox/<task>/` holds each task's answers, one file per run.
- `autobot log` lists recent runs. `autobot show <task>` shows the latest run of a task, with every tool call. `autobot show <run> --full` includes the system prompt.

For notifications, set `notify.method` in `config.toml` to `notify-send`, `ntfy` or `email`, and `notify.on` to `always` or `failure`.

## Memory

After each run, autobot asks the model what is worth remembering and applies the changes to two files:

- `~/.local/state/autobot/memory/<task>.md` for that task,
- `~/.local/state/autobot/memory/shared.md` for every task.

Both are Markdown and safe to edit by hand. Every change is a git commit:

```bash
autobot memory diff          # what the last reflection changed
autobot memory diff -n 5     # the last five changes
autobot memory undo          # revert the last change; run again to go further back
```

## Configuration

`autobot init` writes `~/.config/autobot/config.toml` with every setting and its default. The main ones:

| Setting | Default | Meaning |
|---|---|---|
| `provider.url` | | OpenAI-compatible endpoint, such as `http://localhost:11434/v1` |
| `provider.model` | | Model name |
| `provider.api_key_env` | `OPENAI_API_KEY` | Environment variable that holds the key |
| `provider.api_key_command` | | Command that prints the key |
| `provider.timeout` | `5m` | Per-request timeout |
| `provider.context_tokens` | `32000` | The model's context window |
| `defaults.schedule` | `daily` | Schedule for tasks that set none |
| `defaults.max_rounds` | `20` | Tool rounds for tasks that set none |
| `defaults.max_tool_output_tokens` | `3000` | Longer tool outputs are cut |
| `memory.max_bytes` | `8000` | Size limit per memory file |
| `records.keep_days` | `30` | Run records older than this are deleted |
| `notify.method` | `none` | `none`, `notify-send`, `ntfy` or `email` |

`system.md` next to it is the system prompt. Delete it to use the built-in one.

## Commands

```text
autobot init                     Create config, a system prompt, an example task and the systemd timer
autobot check [--offline]        Test config, provider, key and tool calling; list tasks
autobot new <name>               Create a task from a template
autobot run <task>... [--dry-run]
autobot run --due                Run every task that is due (used by the timer)
autobot log [-n N] [task]        List recent runs
autobot show [run] [--full]      Show a run's report and transcript
autobot memory diff [-n N]       Show the last memory changes
autobot memory undo [--force]    Revert the last memory change
```

## Development

```bash
go test ./...
CGO_ENABLED=0 go build ./cmd/autobot   # a static binary with no libc dependency
```
