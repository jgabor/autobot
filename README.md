# autobot.sh

`autobot.sh` is a small Bash script for running AI tasks in sequence.

Tasks are written in plain language and ordered by their filenames:

```text
1-first-task.md
2-another-task.md
3-last-task.md
```

It is intended to run once, or be run periodically by a user-level systemd unit. The one-run task loop, basic local tools, periodic scheduling via systemd, self-check, and self-reflection are in place; web search is still being added.

## Files

autobot is intended to run as a user-level systemd unit.

Configuration and tasks live in:

```text
${XDG_CONFIG_HOME:-$HOME/.config}/autobot/
├── 0-system.md
├── 1-first-task.md
└── 2-another-task.md
```

`0-system.md` is reserved for the system prompt and other system-level instructions. Files starting with `1-`, `2-`, and so on are treated as tasks and run in filename order.

Persistent state lives in:

```text
${XDG_STATE_HOME:-$HOME/.local/state}/autobot/state.md
```

After each run, autobot reflects on what happened and updates the state file with useful locations, procedures, recurring gotchas, durable insights, open follow-ups, and three improvements for the next run.

The state file is a curated memory, not a full run log. It should stay concise and only contain information that is likely to be useful later.

The user-level systemd unit lives in:

```text
${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/autobot.service
```

The unit optionally reads provider settings from:

```text
${XDG_CONFIG_HOME:-$HOME/.config}/autobot/environment
```

The unit expects the script at `$HOME/.local/bin/autobot.sh`.

## Configuration

You need to have the following environment variables set:

- `AUTOBOT_PROVIDER` set to an OpenAI-compatible endpoint (e.g., `http://localhost:11434/v1`).
- `AUTOBOT_MODEL` set to preferred model, e.g. `Qwen3.5-9B`
- `OPENAI_API_KEY` set for OpenAI-compatible endpoints, if required
- `OPENCODE_API_KEY` set for OpenCode endpoints, if required
- `KAGI_API_KEY` set for web search once that tool is added

Optional:

- `AUTOBOT_TIMEOUT` per-request timeout in seconds (default: 300)
- `AUTOBOT_MAX_TOOL_ROUNDS` maximum tool-call rounds per task (default: 20)
- `AUTOBOT_MAX_TOOL_OUTPUT` maximum characters kept per tool output (default: 12000)
- `AUTOBOT_MAX_TOOL_RESULTS` maximum results per Grep or Glob call (default: 500)

Example:

```bash
AUTOBOT_PROVIDER=https://api.example.com
AUTOBOT_MODEL=Qwen/Qwen3.5-9B
OPENAI_API_KEY=sk-...
KAGI_API_KEY=...
```

## TODO

- Use Kagi for web search, or update the README to match the chosen search provider.
