---
schedule: weekly
read:
  - ~/.config/systemd/user
commands:
  - systemctl --user list-units --type=service --all --no-pager
  - systemctl --user list-timers --all --no-pager
---
Audit my user-level systemd units.

List the *.service and *.timer files in ~/.config/systemd/user and read up to ten of them. If the folder does not exist, say so and stop. Run the two allowed systemctl commands to see what is loaded and running.

For each unit, summarize what it does and flag units that are stale, duplicated, failing or conflicting. Compare with the findings in memory and say what changed since the last audit.

Reply with the audit report. Do not change any unit files.
