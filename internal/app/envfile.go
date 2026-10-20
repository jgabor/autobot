package app

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnvironmentFile is where API keys can live, so manual runs and timer runs
// see the same variables. The systemd unit reads the same file.
func (a *App) EnvironmentFile() string { return filepath.Join(a.Paths.ConfigDir, "environment") }

// ParseEnvFile reads KEY=VALUE lines, skipping blanks and # comments.
// Surrounding single or double quotes are removed from values.
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		vars[k] = v
	}
	return vars, sc.Err()
}

// LoadEnvironment sets variables from the environment file that are not
// already set. A missing file is fine.
func (a *App) LoadEnvironment() error {
	vars, err := ParseEnvFile(a.EnvironmentFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for k, v := range vars {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}
