package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jgabor/autobot/internal/paths"
)

type moveArgs struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Into  string `json:"into"`
	Merge bool   `json:"merge"`
}

// Move is one file or folder the tools moved, or would move in a dry run.
type Move struct {
	From string
	To   string
}

// Moved lists the moves in the order they were made.
func (s *Set) Moved() []Move {
	return append([]Move(nil), s.moves...)
}

// lexical expands a tool path without resolving symlinks.
func (s *Set) lexical(path string) string {
	path = paths.Expand(path, s.o.Home)
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.o.Policy.Output(), path)
	}
	return filepath.Clean(path)
}

func (s *Set) move(a moveArgs) (string, error) {
	// into names the folder outright, so a missing trailing / cannot turn
	// "move into Code" into "rename to Code".
	if a.Into != "" {
		if a.To != "" || a.Merge {
			return "", errors.New("give into on its own, without to or merge")
		}
		a.To = strings.TrimSuffix(a.Into, "/") + "/"
	}
	if a.From == "" || a.To == "" {
		return "", errors.New("from and to (or into) are required")
	}
	from, err := s.o.Policy.CheckWrite(a.From)
	if err != nil {
		return "", err
	}
	// Moving a link would move whatever it points to.
	if info, err := os.Lstat(s.lexical(a.From)); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symbolic link; links are not moved", s.tilde(s.lexical(a.From)))
	}
	src, err := os.Stat(from)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s does not exist", s.tilde(from))
	}
	if err != nil {
		return "", err
	}
	if !src.IsDir() && !src.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", s.tilde(from))
	}
	to, err := s.o.Policy.Resolve(a.To)
	if err != nil {
		return "", fmt.Errorf("access denied: %s: %w", a.To, err)
	}
	toInfo, err := os.Stat(to)
	toIsDir := err == nil && toInfo.IsDir()
	if a.Merge {
		if !src.IsDir() || !toIsDir {
			return "", errors.New("merge needs two existing folders")
		}
		return s.merge(from, to)
	}
	// Like mv: a file or folder moved to a folder goes inside it.
	dest := to
	if toIsDir || strings.HasSuffix(a.To, "/") {
		dest = filepath.Join(to, filepath.Base(from))
	}
	if dest, err = s.o.Policy.CheckWrite(dest); err != nil {
		return "", err
	}
	if dest == from {
		return "", errors.New("from and to are the same path")
	}
	if strings.HasPrefix(dest, from+"/") {
		return "", fmt.Errorf("cannot move %s into itself", s.tilde(from))
	}
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("%s already exists; it was not overwritten", s.tilde(dest))
	}
	if err := s.rename(from, dest); err != nil {
		return "", err
	}
	return fmt.Sprintf("Moved %s to %s", s.tilde(from), s.tilde(dest)) + s.dryRunNote(), nil
}

// merge moves everything in the folder from into the existing folder to, then
// removes from. If any name already exists in to, nothing is moved.
func (s *Set) merge(from, to string) (string, error) {
	if to == from || strings.HasPrefix(to, from+"/") {
		return "", fmt.Errorf("cannot move %s into itself", s.tilde(from))
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return "", err
	}
	var clashes []string
	dests := make([]string, len(entries))
	for i, e := range entries {
		dest, err := s.o.Policy.CheckWrite(filepath.Join(to, e.Name()))
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(dest); err == nil {
			clashes = append(clashes, e.Name())
		}
		dests[i] = dest
	}
	if len(clashes) > 0 {
		if len(clashes) > 10 {
			clashes = append(clashes[:10], fmt.Sprintf("and %d more", len(clashes)-10))
		}
		return "", fmt.Errorf("nothing was moved: these names already exist in %s: %s", s.tilde(to), strings.Join(clashes, ", "))
	}
	for i, e := range entries {
		if err := s.rename(filepath.Join(from, e.Name()), dests[i]); err != nil {
			return "", fmt.Errorf("moved %d of %d entries into %s, then failed: %w", i, len(entries), s.tilde(to), err)
		}
	}
	if !s.o.DryRun {
		if err := os.Remove(from); err != nil {
			return "", fmt.Errorf("moved all %d entries into %s, but could not remove %s: %w", len(entries), s.tilde(to), s.tilde(from), err)
		}
	}
	return fmt.Sprintf("Moved %d entries from %s into %s and removed the empty folder", len(entries), s.tilde(from), s.tilde(to)) + s.dryRunNote(), nil
}

// rename moves one path, creating missing folders where the policy allows. In
// a dry run it only records the move.
func (s *Set) rename(from, dest string) error {
	dir := filepath.Dir(dest)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) && !s.o.Policy.CanCreateDir(dir) {
		return fmt.Errorf("folder %s does not exist and cannot be created; missing folders are only created inside the output folder or a folder listed under `write:`", s.tilde(dir))
	}
	if !s.o.DryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.Rename(from, dest); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				return fmt.Errorf("%s and %s are on different file systems; move cannot copy between them", s.tilde(from), s.tilde(dir))
			}
			return err
		}
	}
	s.moves = append(s.moves, Move{From: from, To: dest})
	return nil
}

// dryRunNote tells the model why later listings still show the old layout.
func (s *Set) dryRunNote() string {
	if !s.o.DryRun {
		return ""
	}
	return " (dry run: recorded but not done, so listings still show the old layout; treat the move as done)"
}

func (s *Set) tilde(p string) string { return paths.Tilde(p, s.o.Home) }
