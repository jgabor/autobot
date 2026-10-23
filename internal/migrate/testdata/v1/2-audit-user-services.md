Audit the user-level systemd units in `~/.config/systemd/user`.

Glob for `~/.config/systemd/user/*.service` and read up to ten unit files. If the directory does not exist, say so and stop. For each unit, summarize what it does and flag stale, duplicated, or conflicting entries. Check `state.md` for findings from prior runs and update them if the situation has changed.

Write the audit report to `~/.config/systemd/user/audit.md`. Do not modify or restart any units.
