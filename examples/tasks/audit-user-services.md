---
schedule: weekly
read:
  - ~/.config/systemd/user
commands:
  - systemctl --user list-units --type=service --no-pager
  - systemctl --user list-units --state=failed --no-pager
  - systemctl --user list-timers --all --no-pager
---
Audit my user-level systemd units.

List the *.service and *.timer files in ~/.config/systemd/user and read up to ten of them. If the folder does not exist, say so and stop. Run the allowed systemctl commands to see which services are running, which units have failed and which timers are scheduled.

For each unit, summarize what it does and flag units that are stale, duplicated, failing or conflicting. Compare with the findings in memory and say what changed since the last audit.

Reply with the audit report. Do not change any unit files.
