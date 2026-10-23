package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"time"

	"github.com/jgabor/autobot/internal/paths"
)

const (
	maxGrepFileBytes = 10 << 20
	// Longer lines are skipped, which bounds the work of one match.
	maxGrepLine = 64 << 10
	// Patterns that compile to more instructions than this are refused.
	maxRegexpInsts = 1000
	// A walk over a huge or slow tree, such as a network share, stops at
	// these limits instead of holding up the run.
	walkTimeout    = time.Minute
	maxWalkEntries = 200000
)

// guard stops a folder walk on cancellation, timeout or too many entries.
type guard struct {
	ctx      context.Context
	deadline time.Time
	n        int
	stopped  string
}

func newGuard(ctx context.Context) *guard {
	return &guard{ctx: ctx, deadline: time.Now().Add(walkTimeout)}
}

func (g *guard) stop() bool {
	g.n++
	switch {
	case g.n > maxWalkEntries:
		g.stopped = fmt.Sprintf("stopped after %d entries", maxWalkEntries)
	case g.n%256 != 1:
		return false
	case g.ctx.Err() != nil:
		g.stopped = "stopped because the run was cancelled"
	case time.Now().After(g.deadline):
		g.stopped = fmt.Sprintf("stopped after %s", walkTimeout)
	}
	return g.stopped != ""
}

// expired checks only for cancellation and timeout. Long scans inside one
// file call it, since lines are not walk entries.
func (g *guard) expired() bool {
	switch {
	case g.stopped != "":
	case g.ctx.Err() != nil:
		g.stopped = "stopped because the run was cancelled"
	case time.Now().After(g.deadline):
		g.stopped = fmt.Sprintf("stopped after %s", walkTimeout)
	}
	return g.stopped != ""
}

func (g *guard) note() string {
	if g.stopped == "" {
		return ""
	}
	return "\n[search " + g.stopped + "; results are incomplete. Use a narrower path]"
}

type listArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Include    string `json:"include"`
	IgnoreCase bool   `json:"ignore_case"`
}

func hasMeta(s string) bool { return strings.ContainsAny(s, "*?[") }

// splitPattern moves the fixed leading folders of an absolute pattern into
// the root, so ~/Notes/*.md walks ~/Notes rather than /.
func splitPattern(root, pattern, home string) (string, string) {
	pattern = paths.Expand(pattern, home)
	if !filepath.IsAbs(pattern) {
		return root, pattern
	}
	segs := strings.Split(filepath.Clean(pattern), "/")
	i := 1
	for i < len(segs) && !hasMeta(segs[i]) {
		i++
	}
	return "/" + strings.Join(segs[1:i], "/"), strings.Join(segs[i:], "/")
}

// matchSegments matches a slash-separated path against a glob where **
// stands for any number of folders. It memoizes, so many ** segments stay
// fast instead of taking exponential time.
func matchSegments(pat, name []string) bool {
	memo := map[[2]int]bool{}
	var match func(pi, ni int) bool
	match = func(pi, ni int) bool {
		key := [2]int{pi, ni}
		if v, ok := memo[key]; ok {
			return v
		}
		var ok bool
		switch {
		case pi == len(pat):
			ok = ni == len(name)
		case pat[pi] == "**":
			ok = match(pi+1, ni) || (ni < len(name) && match(pi, ni+1))
		case ni == len(name):
			ok = false
		default:
			m, err := path.Match(pat[pi], name[ni])
			ok = err == nil && m && match(pi+1, ni+1)
		}
		memo[key] = ok
		return ok
	}
	return match(0, 0)
}

func (s *Set) list(ctx context.Context, a listArgs) (string, error) {
	if a.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	if strings.Contains(a.Pattern, "{") {
		return "", errors.New("brace patterns such as {a,b} are not supported; make one call per pattern")
	}
	if a.Path == "" {
		a.Path = "."
	}
	rootArg, pattern := splitPattern(a.Path, a.Pattern, s.o.Home)
	root, err := s.o.Policy.CheckRead(rootArg)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("%s does not exist", paths.Tilde(root, s.o.Home))
	}
	if !info.IsDir() {
		if pattern == "" {
			return paths.Tilde(root, s.o.Home), nil
		}
		return "", fmt.Errorf("%s is not a folder", paths.Tilde(root, s.o.Home))
	}
	if pattern == "" {
		pattern = "*"
	}
	pat := strings.Split(strings.Trim(pattern, "/"), "/")
	for _, p := range pat {
		if _, err := path.Match(p, ""); err != nil {
			return "", fmt.Errorf("invalid pattern %q: %w", a.Pattern, err)
		}
	}
	maxDepth := len(pat)
	for _, p := range pat {
		if p == "**" {
			maxDepth = -1
		}
	}

	var matches []string
	seen := map[string]bool{}
	limited := false
	errStop := errors.New("stop")
	g := newGuard(ctx)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if p == root {
			return nil
		}
		if g.stop() {
			return filepath.SkipAll
		}
		if !s.o.Policy.CanRead(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		segs := strings.Split(rel, "/")
		if matchSegments(pat, segs) {
			if len(matches) >= s.o.MaxResults {
				limited = true
				return errStop
			}
			name := paths.Tilde(p, s.o.Home)
			if d.IsDir() {
				name += "/"
			}
			matches = append(matches, name)
			seen[p] = true
		}
		if d.IsDir() && maxDepth >= 0 && len(segs) >= maxDepth {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return "", err
	}
	// Files written earlier in a dry run exist only in the overlay.
	for p := range s.overlay {
		rel, err := filepath.Rel(root, p)
		if err != nil || seen[p] || strings.HasPrefix(rel, "..") || len(matches) >= s.o.MaxResults {
			continue
		}
		if matchSegments(pat, strings.Split(rel, "/")) {
			matches = append(matches, paths.Tilde(p, s.o.Home))
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("No matches for %s in %s", pattern, paths.Tilde(root, s.o.Home)) + g.note(), nil
	}
	sort.Strings(matches)
	out := strings.Join(matches, "\n")
	if limited {
		out += fmt.Sprintf("\n[results limited to %d; use a narrower pattern]", s.o.MaxResults)
	}
	return out + g.note(), nil
}

func (s *Set) grep(ctx context.Context, a grepArgs) (string, error) {
	if a.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid regular expression: %w", err)
	}
	if err := checkComplexity(expr); err != nil {
		return "", err
	}
	if a.Include != "" {
		if _, err := path.Match(a.Include, ""); err != nil {
			return "", fmt.Errorf("invalid include pattern: %w", err)
		}
	}
	if a.Path == "" {
		a.Path = "."
	}
	root, err := s.o.Policy.CheckRead(a.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	_, pending := s.overlay[root]
	if err != nil && !pending {
		return "", fmt.Errorf("%s does not exist", paths.Tilde(root, s.o.Home))
	}

	var out []string
	limited := false
	g := newGuard(ctx)
	searchFile := func(p string) {
		if limited {
			return
		}
		if a.Include != "" {
			if ok, _ := path.Match(a.Include, filepath.Base(p)); !ok {
				return
			}
		}
		text, ok := s.overlay[p]
		if !ok {
			fi, err := os.Stat(p)
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxGrepFileBytes {
				return
			}
			data, err := os.ReadFile(p)
			if err != nil || isBinary(data) {
				return
			}
			text = string(data)
		}
		sc := bufio.NewScanner(strings.NewReader(text))
		sc.Buffer(make([]byte, 64*1024), maxGrepLine)
		for n := 1; sc.Scan(); n++ {
			if n%16 == 0 && g.expired() {
				return
			}
			line := sc.Text()
			if !re.MatchString(line) {
				continue
			}
			if len(out) >= s.o.MaxResults {
				limited = true
				return
			}
			if len(line) > 300 {
				line = line[:300] + "..."
			}
			out = append(out, fmt.Sprintf("%s:%d: %s", paths.Tilde(p, s.o.Home), n, line))
		}
	}

	if pending || !info.IsDir() {
		searchFile(root)
	} else {
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == root {
					return err
				}
				return nil
			}
			if limited || g.stopped != "" || g.stop() {
				return filepath.SkipAll
			}
			if !s.o.Policy.CanRead(p) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() {
				searchFile(p)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		// Files written earlier in a dry run exist only in the overlay.
		for p := range s.overlay {
			rel, err := filepath.Rel(root, p)
			if _, statErr := os.Stat(p); err == nil && !strings.HasPrefix(rel, "..") && statErr != nil {
				searchFile(p)
			}
		}
	}
	if len(out) == 0 {
		return "No matches found." + g.note(), nil
	}
	result := strings.Join(out, "\n")
	if limited {
		result += fmt.Sprintf("\n[results limited to %d lines; narrow the pattern or path]", s.o.MaxResults)
	}
	return result + g.note(), nil
}

// checkComplexity refuses patterns whose compiled program is so large that
// matching could take minutes, such as nested counted repetitions.
func checkComplexity(expr string) error {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return fmt.Errorf("invalid regular expression: %w", err)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return fmt.Errorf("invalid regular expression: %w", err)
	}
	if len(prog.Inst) > maxRegexpInsts {
		return fmt.Errorf("the regular expression is too complex (%d steps, limit %d); simplify it", len(prog.Inst), maxRegexpInsts)
	}
	return nil
}
