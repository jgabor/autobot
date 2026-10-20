// Package access decides which paths a task may read and write.
//
// Every path is expanded, made absolute and resolved through symlinks before
// it is compared with the task's declared roots. Comparison is exact and case
// sensitive, by whole path components.
package access

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jgabor/autobot/internal/paths"
)

// Options describe one task's access.
type Options struct {
	Home string
	// Output is the task's own folder. It is always readable and writable,
	// and relative paths resolve against it.
	Output string
	Read   []string
	Write  []string
	// Protected roots cannot be read or written by tools, except for Output.
	Protected []string
}

// Policy enforces Options.
type Policy struct {
	home      string
	output    string
	outputLex string
	read      []root
	write     []root
	// protected holds resolved paths: the protected roots and the targets
	// of symlinks inside them. protectedLex holds the roots as written.
	protected    []string
	protectedLex []string
}

// root is one declared path. A folder root covers everything inside it; a
// file root covers only itself. A root is a folder if it exists as one, or
// if it is written with a trailing slash.
type root struct {
	display  string
	resolved string
	dir      bool
}

func newRoot(display, resolved string) root {
	dir := strings.HasSuffix(display, "/")
	if info, err := os.Stat(resolved); err == nil {
		dir = info.IsDir()
	}
	return root{display, resolved, dir}
}

func (r root) covers(path string) bool {
	if r.dir {
		return within(r.resolved, path)
	}
	return path == r.resolved
}

// New resolves all roots once so later checks compare like with like.
func New(o Options) (*Policy, error) {
	p := &Policy{home: o.Home}
	out, err := p.resolve(o.Output, "/")
	if err != nil {
		return nil, fmt.Errorf("output folder: %w", err)
	}
	p.output = out
	p.outputLex = p.lexical(o.Output, "/")
	for _, r := range o.Read {
		res, err := p.resolve(r, "/")
		if err != nil {
			return nil, fmt.Errorf("read path %s: %w", r, err)
		}
		p.read = append(p.read, newRoot(r, res))
	}
	for _, w := range o.Write {
		res, err := p.resolve(w, "/")
		if err != nil {
			return nil, fmt.Errorf("write path %s: %w", w, err)
		}
		p.write = append(p.write, newRoot(w, res))
	}
	for _, pr := range o.Protected {
		res, err := p.resolve(pr, "/")
		if err != nil {
			return nil, fmt.Errorf("protected path %s: %w", pr, err)
		}
		p.protected = append(p.protected, res)
		p.protectedLex = append(p.protectedLex, p.lexical(pr, "/"))
		// A symlink inside a protected folder, as dotfile managers create,
		// points at a file that must be just as protected. /proc is full of
		// links to other processes' folders and the root, so it is skipped.
		if !within("/proc", res) {
			p.protected = append(p.protected, linkTargets(res, 3)...)
		}
	}
	return p, nil
}

// linkTargets returns the resolved targets of symlinks under dir, down to
// depth levels. It skips the task output folders, which hold task files.
func linkTargets(dir string, depth int) []string {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil || depth == 0 {
		return nil
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.Type()&fs.ModeSymlink != 0 {
			if target, err := filepath.EvalSymlinks(p); err == nil {
				out = append(out, target)
			}
			continue
		}
		if e.IsDir() && e.Name() != "inbox" && e.Name() != "runs" && e.Name() != ".git" {
			out = append(out, linkTargets(p, depth-1)...)
		}
	}
	return out
}

// lexical expands and cleans a path without resolving symlinks.
func (p *Policy) lexical(path, base string) string {
	path = paths.Expand(path, p.home)
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}

// Output returns the resolved output folder.
func (p *Policy) Output() string { return p.output }

// ReadPaths lists the read roots as written in the task, for prompts and errors.
func (p *Policy) ReadPaths() []string { return displays(p.read) }

// WritePaths lists the write roots as written in the task.
func (p *Policy) WritePaths() []string { return displays(p.write) }

func displays(rs []root) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.display
	}
	return out
}

// Resolve turns a tool's path argument into an absolute path with symlinks
// resolved, without checking access.
func (p *Policy) Resolve(path string) (string, error) {
	return p.resolve(path, p.output)
}

func (p *Policy) resolve(path, base string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	if strings.ContainsRune(path, 0) {
		return "", errors.New("path contains a NUL byte")
	}
	path = paths.Expand(path, p.home)
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return resolveExisting(filepath.Clean(path))
}

// resolveExisting resolves symlinks in the longest existing prefix of path
// and appends the rest. This covers new files under a symlinked folder.
func resolveExisting(path string) (string, error) {
	var rest []string
	cur := path
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

func within(root, path string) bool {
	if root == "/" {
		return true
	}
	return path == root || strings.HasPrefix(path, root+"/")
}

// isProtectedLex catches paths that only reach outside a protected folder
// through a symlink inside it.
func (p *Policy) isProtectedLex(lex string) bool {
	if within(p.outputLex, lex) || within(p.output, lex) {
		return false
	}
	for _, pr := range p.protectedLex {
		if within(pr, lex) {
			return true
		}
	}
	return false
}

func (p *Policy) isProtected(path string) bool {
	if within(p.output, path) {
		return false
	}
	for _, pr := range p.protected {
		if within(pr, path) {
			return true
		}
	}
	return false
}

// CheckRead returns the resolved path if the task may read it.
func (p *Policy) CheckRead(path string) (string, error) {
	return p.check(path, "read", p.read)
}

// CheckWrite returns the resolved path if the task may write it. A folder
// root itself cannot be written as a file.
func (p *Policy) CheckWrite(path string) (string, error) {
	resolved, err := p.check(path, "write", p.write)
	if err != nil {
		return "", err
	}
	for _, r := range p.write {
		if r.dir && resolved == r.resolved {
			return "", fmt.Errorf("%s is a folder listed under `write:`; write files inside it", paths.Tilde(resolved, p.home))
		}
	}
	return resolved, nil
}

// CanRead reports whether an already resolved path is readable.
func (p *Policy) CanRead(resolved string) bool {
	return p.allowed(resolved, p.read)
}

func (p *Policy) allowed(resolved string, roots []root) bool {
	if p.isProtected(resolved) {
		return false
	}
	if within(p.output, resolved) {
		return true
	}
	for _, r := range roots {
		if r.covers(resolved) {
			return true
		}
	}
	return false
}

// CanCreateDir reports whether missing folders may be created at the
// already resolved path dir: only inside the output folder or a folder root.
func (p *Policy) CanCreateDir(dir string) bool {
	if p.isProtected(dir) {
		return false
	}
	if within(p.output, dir) {
		return true
	}
	for _, r := range p.write {
		if r.dir && within(r.resolved, dir) {
			// Create the declared folder and below, never its parents.
			_, err := os.Stat(filepath.Dir(r.resolved))
			return err == nil
		}
	}
	return false
}

func (p *Policy) check(path, op string, roots []root) (string, error) {
	resolved, err := p.Resolve(path)
	if err != nil {
		return "", fmt.Errorf("access denied: %s: %w", path, err)
	}
	if p.isProtected(resolved) || p.isProtectedLex(p.lexical(path, p.output)) {
		return "", fmt.Errorf("access denied: %s is one of autobot's own files, which tasks cannot %s", paths.Tilde(resolved, p.home), op)
	}
	if p.allowed(resolved, roots) {
		return resolved, nil
	}
	allowed := []string{paths.Tilde(p.output, p.home) + " (output folder)"}
	allowed = append(allowed, displays(roots)...)
	return "", fmt.Errorf("access denied: %s is outside this task's %s paths (%s). Add it to `%s:` in the task header if the task needs it",
		paths.Tilde(resolved, p.home), op, strings.Join(allowed, ", "), op)
}
