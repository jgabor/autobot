// Package task parses task files: a YAML header followed by a Markdown prompt.
package task

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jgabor/autobot/internal/schedule"
	"gopkg.in/yaml.v3"
)

// Task is one parsed task file.
type Task struct {
	ID        string
	Path      string
	Schedule  schedule.Schedule
	Read      []string
	Write     []string
	Network   bool
	Commands  []string
	Model     string
	MaxRounds int
	Enabled   bool
	Prompt    string
}

// Defaults fill in header fields a task leaves out.
type Defaults struct {
	Schedule  string
	MaxRounds int
}

type header struct {
	Schedule  string   `yaml:"schedule"`
	Read      pathList `yaml:"read"`
	Write     pathList `yaml:"write"`
	Network   bool     `yaml:"network"`
	Commands  []string `yaml:"commands"`
	Model     string   `yaml:"model"`
	MaxRounds int      `yaml:"max_rounds"`
	Enabled   *bool    `yaml:"enabled"`
}

// pathList is a list of paths in which a bare ~ means the home folder. In
// YAML a bare ~ is null, which would otherwise drop the entry silently.
type pathList []string

func (l *pathList) UnmarshalYAML(n *yaml.Node) error {
	item := func(n *yaml.Node) (string, error) {
		if n.Kind != yaml.ScalarNode {
			return "", fmt.Errorf("line %d: a path must be a single value", n.Line)
		}
		if n.Tag == "!!null" {
			if n.Value == "~" {
				return "~", nil
			}
			return "", fmt.Errorf("line %d: empty path", n.Line)
		}
		return n.Value, nil
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, c := range n.Content {
			v, err := item(c)
			if err != nil {
				return err
			}
			*l = append(*l, v)
		}
		return nil
	case yaml.ScalarNode:
		if n.Tag == "!!null" && n.Value != "~" {
			return nil
		}
		v, err := item(n)
		if err != nil {
			return err
		}
		*l = pathList{v}
		return nil
	}
	return fmt.Errorf("line %d: expected a list of paths", n.Line)
}

var bareTildeField = regexp.MustCompile(`(?m)^(read|write):[ \t]*~[ \t]*(#.*)?$`)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// reserved names are used by autobot's own files: shared memory and the
// digest folder in the inbox.
var reserved = map[string]bool{"shared": true, "digests": true}

// ValidID reports whether id can name a task.
func ValidID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("task name %q must use lowercase letters, digits, - and _, and start with a letter or digit", id)
	}
	if reserved[id] {
		return fmt.Errorf("task name %q is reserved", id)
	}
	return nil
}

// Parse reads a task from its file name and content.
func Parse(path string, data []byte, d Defaults) (*Task, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".md")
	if err := ValidID(id); err != nil {
		return nil, err
	}
	head, body, err := splitHeader(data)
	if err != nil {
		return nil, err
	}
	var h header
	// yaml.v3 skips a null field without calling its unmarshaler, so a
	// whole field written as a bare ~ is turned into a one-item list first.
	head = bareTildeField.ReplaceAll(head, []byte(`$1: ["~"] $2`))
	if len(bytes.TrimSpace(head)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(head))
		dec.KnownFields(true)
		// A header with only comments decodes to EOF; that is an empty header.
		if err := dec.Decode(&h); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("header: %w", err)
		}
	}
	t := &Task{
		ID:        id,
		Path:      path,
		Read:      h.Read,
		Write:     h.Write,
		Network:   h.Network,
		Commands:  h.Commands,
		Model:     h.Model,
		MaxRounds: h.MaxRounds,
		Enabled:   h.Enabled == nil || *h.Enabled,
		Prompt:    strings.TrimSpace(string(body)),
	}
	spec := h.Schedule
	if spec == "" {
		spec = d.Schedule
	}
	if t.Schedule, err = schedule.Parse(spec); err != nil {
		return nil, err
	}
	if t.MaxRounds == 0 {
		t.MaxRounds = d.MaxRounds
	}
	if t.MaxRounds < 0 {
		return nil, errors.New("max_rounds must be positive")
	}
	if t.Prompt == "" {
		return nil, errors.New("the task has no prompt below the header")
	}
	for _, field := range []struct {
		name  string
		paths []string
	}{{"read", t.Read}, {"write", t.Write}} {
		for _, p := range field.paths {
			if !strings.HasPrefix(p, "/") && p != "~" && !strings.HasPrefix(p, "~/") {
				return nil, fmt.Errorf("%s path %q must start with / or ~/", field.name, p)
			}
		}
	}
	for _, c := range t.Commands {
		argv, err := SplitCommand(c)
		if err != nil {
			return nil, fmt.Errorf("command %q: %w", c, err)
		}
		if len(argv) == 0 {
			return nil, errors.New("commands must not contain empty entries")
		}
	}
	return t, nil
}

func splitHeader(data []byte) (head, body []byte, err error) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	// Leading blank lines and trailing spaces on the --- lines are allowed,
	// so a small slip does not turn the header into part of the prompt.
	first := 0
	for first < len(lines) && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	if first == len(lines) || strings.TrimRight(lines[first], " \t") != "---" {
		return nil, []byte(text), nil
	}
	for i := first + 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			return []byte(strings.Join(lines[first+1:i], "\n")), []byte(strings.Join(lines[i+1:], "\n")), nil
		}
	}
	return nil, nil, errors.New("header starts with --- but has no closing ---")
}

// Load reads one task file.
func Load(path string, d Defaults) (*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := Parse(path, data, d)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return t, nil
}

// Invalid is a task file that failed to parse.
type Invalid struct {
	ID   string
	Path string
	Err  error
}

// LoadAll reads every *.md file in dir, sorted by name. Files that fail to
// parse are returned separately so one bad task does not hide the others.
func LoadAll(dir string, d Defaults) ([]*Task, []Invalid, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("no tasks folder at %s (run `autobot init`)", dir)
	}
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var tasks []*Task
	var invalid []Invalid
	for _, name := range names {
		path := filepath.Join(dir, name)
		t, err := Load(path, d)
		if err != nil {
			invalid = append(invalid, Invalid{ID: strings.TrimSuffix(name, ".md"), Path: path, Err: err})
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks, invalid, nil
}

// SplitCommand splits a command line into arguments the way a shell would
// for plain words and quotes. It does not expand variables, globs or
// anything else, and rejects shell operators.
func SplitCommand(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unclosed single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unclosed double quote")
			}
			inWord = true
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New("trailing backslash")
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case strings.IndexByte("|&;<>()$`", c) >= 0:
			return nil, fmt.Errorf("shell syntax %q is not supported; list the exact command and its arguments", string(c))
		case c == '~' && !inWord:
			return nil, errors.New("~ is not expanded in commands; use the full path")
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}
