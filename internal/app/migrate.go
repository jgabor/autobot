package app

import (
	"github.com/jgabor/autobot/internal/migrate"
)

// Migrate converts a v1 setup in from (default: the config folder).
func (a *App) Migrate(from string) error {
	res, err := migrate.Run(migrate.Options{
		Paths: a.Paths,
		From:  from,
		Env:   a.Env,
		Log:   func(format string, args ...any) { a.printf(format+"\n", args...) },
	})
	if err != nil {
		return err
	}
	if !res.Anything {
		a.printf("Nothing to migrate: no v1 task files (1-name.md), 0-system.md, AUTOBOT_* settings or state.md found.\n")
		return nil
	}
	a.printf(`
Migrated tasks are disabled until you review them. For each task:
  1. Check the read and write paths marked "# review" in its header.
  2. Try it without writing anything: autobot run <name> --dry-run
  3. Set enabled: true.

To replace the v1 service with the v2 timer:
  systemctl --user disable --now autobot.service
  rm %s/autobot.service
  autobot init
  systemctl --user daemon-reload
  systemctl --user enable --now autobot.timer
`, a.tilde(a.Paths.SystemdDir()))
	return nil
}
