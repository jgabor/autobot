package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/jgabor/autobot/internal/task"
)

const maxCommandOutput = 1 << 20

type commandArgs struct {
	Command string `json:"command"`
}

// commandEnv passes only what common read-only commands need. systemctl
// --user needs XDG_RUNTIME_DIR and the session bus address.
func commandEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (s *Set) runCommand(ctx context.Context, a commandArgs) (string, error) {
	cmd := strings.TrimSpace(a.Command)
	allowed := false
	for _, c := range s.o.Commands {
		if strings.TrimSpace(c) == cmd {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("command not allowed: %q. Allowed commands, matched exactly:\n- %s", cmd, strings.Join(s.o.Commands, "\n- "))
	}
	argv, err := task.SplitCommand(cmd)
	if err != nil || len(argv) == 0 {
		return "", fmt.Errorf("cannot parse command %q: %v", cmd, err)
	}
	// Commands start in an empty folder that only exists for this call, so
	// a command cannot pick up settings or hooks the model wrote, such as a
	// planted .git folder in the output folder.
	dir, err := os.MkdirTemp("", "autobot-cmd-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, s.o.CommandTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = commandEnv()
	c.Dir = dir
	// Kill the whole process group on timeout, and stop waiting for output
	// soon after, so a child process cannot keep the call open.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second
	var buf limitedBuffer
	buf.max = maxCommandOutput
	c.Stdout = &buf
	c.Stderr = &buf
	err = c.Run()
	out := buf.String()
	if buf.cut {
		out += "\n[output cut at 1 MiB]"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("command timed out after %s", s.o.CommandTimeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("%s\n[exit status %d]", out, exitErr.ExitCode()), nil
	}
	if err != nil {
		return "", fmt.Errorf("command failed to start: %w", err)
	}
	if out == "" {
		return "(no output)", nil
	}
	return out, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
	cut bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		b.cut = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
