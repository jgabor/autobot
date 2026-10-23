package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jgabor/autobot/internal/config"
	"github.com/jgabor/autobot/internal/task"
)

const helloTask = `---
schedule: weekly
---
Show that the setup works.

1. Write a file named hello.md in your output folder containing one line: the current date and time.
2. Read the file back to confirm it was written.
3. Reply with two or three sentences: what you did, and what you can access in this run.
`

const newTaskTemplate = `---
# Set to true when the task is ready to run on the timer.
enabled: false
# When to run: hourly, daily, weekly, or a cron expression such as "0 9 * * 1".
schedule: daily
# Folders or files the task may read. The output folder is always allowed.
# End a folder that does not exist yet with /.
# read:
#   - ~/Documents/Notes
# Folders or files the task may write, beyond its output folder.
# write:
#   - ~/Documents/Notes/next-actions.md
# Allow web_search and web_fetch.
# network: true
# Exact, read-only commands the task may run.
# commands:
#   - systemctl --user list-units --type=service
---
Describe the chore in plain language. Tasks work best when they:

- name the exact folders or files to look at,
- limit how much to read, such as "read up to ten files",
- say what to do if a folder is missing,
- say what the answer should contain, and where to write a file if the inbox is not enough,
- say what must not be changed.
`

const timerUnit = `[Unit]
Description=autobot: check for due tasks every 15 minutes

[Timer]
OnCalendar=*:0/15
Persistent=true
RandomizedDelaySec=30

[Install]
WantedBy=timers.target
`

func (a *App) serviceUnit() string {
	exe := a.Executable
	if strings.ContainsAny(exe, " \t\"") {
		exe = `"` + strings.ReplaceAll(exe, `"`, `\"`) + `"`
	}
	return fmt.Sprintf(`[Unit]
Description=autobot: run due tasks

[Service]
Type=oneshot
ExecStart=%s run --due
EnvironmentFile=-%s
Nice=10
# A oneshot unit has no start timeout by default; stop a run that hangs.
TimeoutStartSec=3h
`, exe, a.EnvironmentFile())
}

// createFile writes content unless path exists. It reports what it did.
func (a *App) createFile(path, content string, mode os.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		a.printf("exists, left unchanged: %s\n", a.tilde(path))
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	a.printf("created: %s\n", a.tilde(path))
	return nil
}

// Init creates the config, a default system prompt, an example task and the
// systemd units. It never overwrites files.
func (a *App) Init() error {
	files := []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{a.Paths.ConfigFile(), config.Template, 0o600},
		{a.Paths.SystemFile(), DefaultSystem, 0o644},
		{filepath.Join(a.Paths.TasksDir(), "hello.md"), helloTask, 0o644},
		{filepath.Join(a.Paths.SystemdDir(), "autobot.service"), a.serviceUnit(), 0o644},
		{filepath.Join(a.Paths.SystemdDir(), "autobot.timer"), timerUnit, 0o644},
	}
	for _, f := range files {
		if err := a.createFile(f.path, f.content, f.mode); err != nil {
			return err
		}
	}
	a.printf(`
Next steps:
  1. Set your provider and model:   $EDITOR %s
     If it needs a key, put OPENAI_API_KEY=... in %s (chmod 600).
  2. Test the setup:                autobot check
  3. Try the example task:          autobot run hello
  4. Run tasks on a schedule:       systemctl --user daemon-reload && systemctl --user enable --now autobot.timer
  5. Keep running after logout:     loginctl enable-linger $USER
`, a.tilde(a.Paths.ConfigFile()), a.tilde(a.EnvironmentFile()))
	return nil
}

// NewTask creates a task file from the template.
func (a *App) NewTask(name string) error {
	name = strings.TrimSuffix(name, ".md")
	if err := task.ValidID(name); err != nil {
		return err
	}
	path := filepath.Join(a.Paths.TasksDir(), name+".md")
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists", a.tilde(path))
	}
	if err := a.createFile(path, newTaskTemplate, 0o644); err != nil {
		return err
	}
	a.printf("Edit it, try it without writing anything with `autobot run %s --dry-run`, then set enabled: true.\n", name)
	return nil
}
