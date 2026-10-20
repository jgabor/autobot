// Package paths resolves where autobot keeps its configuration and state.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Paths holds the absolute locations autobot uses.
type Paths struct {
	Home       string
	ConfigHome string
	ConfigDir  string
	StateDir   string
}

// FromEnv builds Paths from HOME, XDG_CONFIG_HOME and XDG_STATE_HOME.
func FromEnv() (Paths, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return Paths{}, errors.New("HOME is not set")
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(configHome) {
		return Paths{}, errors.New("XDG_CONFIG_HOME must be an absolute path")
	}
	if !filepath.IsAbs(stateHome) {
		return Paths{}, errors.New("XDG_STATE_HOME must be an absolute path")
	}
	return Paths{
		Home:       filepath.Clean(home),
		ConfigHome: filepath.Clean(configHome),
		ConfigDir:  filepath.Join(configHome, "autobot"),
		StateDir:   filepath.Join(stateHome, "autobot"),
	}, nil
}

func (p Paths) ConfigFile() string   { return filepath.Join(p.ConfigDir, "config.toml") }
func (p Paths) SystemFile() string   { return filepath.Join(p.ConfigDir, "system.md") }
func (p Paths) TasksDir() string     { return filepath.Join(p.ConfigDir, "tasks") }
func (p Paths) InboxDir() string     { return filepath.Join(p.StateDir, "inbox") }
func (p Paths) MemoryDir() string    { return filepath.Join(p.StateDir, "memory") }
func (p Paths) RunsDir() string      { return filepath.Join(p.StateDir, "runs") }
func (p Paths) ScheduleFile() string { return filepath.Join(p.StateDir, "schedule.json") }
func (p Paths) LockFile() string     { return filepath.Join(p.StateDir, "autobot.lock") }
func (p Paths) SystemdDir() string   { return filepath.Join(p.ConfigHome, "systemd", "user") }

// OutputDir is the folder a task may always write to.
func (p Paths) OutputDir(taskID string) string { return filepath.Join(p.InboxDir(), taskID) }

// Expand replaces a leading ~ with home.
func Expand(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// Tilde shortens a path under home to ~/... for display.
func Tilde(path, home string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(path, home+"/"); ok {
		return "~/" + rel
	}
	return path
}
