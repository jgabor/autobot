package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jgabor/autobot/internal/paths"
)

const maxFileBytes = 20 << 20

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode"`
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// content returns a file's text, preferring dry-run writes. exists is false
// for a file that does not exist on disk or in the overlay.
func (s *Set) content(resolved string) (text string, exists bool, err error) {
	if c, ok := s.overlay[resolved]; ok {
		return c, true, nil
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", true, fmt.Errorf("%s is a folder; use list to see its contents", paths.Tilde(resolved, s.o.Home))
	}
	// Pipes and devices can block forever or never end.
	if !info.Mode().IsRegular() {
		return "", true, fmt.Errorf("%s is not a regular file", paths.Tilde(resolved, s.o.Home))
	}
	if info.Size() > maxFileBytes {
		return "", true, fmt.Errorf("%s is %d bytes, larger than the %d byte limit", paths.Tilde(resolved, s.o.Home), info.Size(), maxFileBytes)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", true, err
	}
	if isBinary(data) {
		return "", true, fmt.Errorf("%s is a binary file", paths.Tilde(resolved, s.o.Home))
	}
	return string(data), true, nil
}

func isBinary(data []byte) bool {
	n := min(len(data), 8000)
	head := data[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	if n < len(data) {
		// The sample may end inside a multi-byte character.
		for i := 0; i < 3 && len(head) > 0 && !utf8.Valid(head); i++ {
			head = head[:len(head)-1]
		}
	}
	return !utf8.Valid(head)
}

func (s *Set) read(a readArgs) (string, error) {
	if a.Offset < 0 {
		return "", errors.New("offset must not be negative")
	}
	if a.Limit == 0 {
		a.Limit = 200
	}
	if a.Limit < 0 || a.Limit > 2000 {
		return "", errors.New("limit must be between 1 and 2000")
	}
	resolved, err := s.o.Policy.CheckRead(a.Path)
	if err != nil {
		return "", err
	}
	text, exists, err := s.content(resolved)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%s does not exist", paths.Tilde(resolved, s.o.Home))
	}
	if text == "" {
		return "(empty file)", nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if a.Offset >= len(lines) {
		return "", fmt.Errorf("offset %d is past the end of the file, which has %d lines", a.Offset, len(lines))
	}
	end := min(a.Offset+a.Limit, len(lines))
	out := strings.Join(lines[a.Offset:end], "")
	if a.Offset > 0 || end < len(lines) {
		out = strings.TrimSuffix(out, "\n") + fmt.Sprintf("\n[lines %d-%d of %d", a.Offset+1, end, len(lines))
		if end < len(lines) {
			out += fmt.Sprintf("; use offset=%d to read more", end)
		}
		out += "]"
	}
	return out, nil
}

func (s *Set) write(a writeArgs) (string, error) {
	switch a.Mode {
	case "", "replace", "w":
		a.Mode = "replace"
	case "append", "a":
		a.Mode = "append"
	default:
		return "", errors.New(`mode must be "replace" or "append"`)
	}
	resolved, err := s.o.Policy.CheckWrite(a.Path)
	if err != nil {
		return "", err
	}
	before, exists, err := s.content(resolved)
	// Replacing a file does not need its old content, so a binary or very
	// large regular file can still be replaced.
	if err != nil && (a.Mode == "append" || !isRegular(resolved)) {
		return "", err
	}
	if size := len(a.Content) + map[bool]int{true: len(before)}[a.Mode == "append"]; size > maxFileBytes {
		return "", fmt.Errorf("the file would be %d bytes, larger than the %d byte limit", size, maxFileBytes)
	}
	after := a.Content
	if a.Mode == "append" {
		after = before + a.Content
	}
	if err := s.store(resolved, after, a.Content, exists, a.Mode == "append"); err != nil {
		return "", err
	}
	verb := "Wrote"
	if a.Mode == "append" {
		verb = "Appended to"
	}
	return fmt.Sprintf("%s %s (%d bytes)", verb, paths.Tilde(resolved, s.o.Home), len(a.Content)), nil
}

// overlayDir reports whether a dry-run write already created dir.
func (s *Set) overlayDir(dir string) bool {
	for p := range s.overlay {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func isRegular(resolved string) bool {
	info, err := os.Stat(resolved)
	return err == nil && info.Mode().IsRegular()
}

func isDirErr(resolved string) bool {
	info, err := os.Stat(resolved)
	return err == nil && info.IsDir()
}

func (s *Set) edit(a editArgs) (string, error) {
	if a.OldString == "" {
		return "", errors.New("old_string must not be empty")
	}
	if a.OldString == a.NewString {
		return "", errors.New("old_string and new_string are the same")
	}
	resolved, err := s.o.Policy.CheckWrite(a.Path)
	if err != nil {
		return "", err
	}
	// Edit reports whether text was found, which would reveal the content
	// of a file the task may write but not read.
	if _, err := s.o.Policy.CheckRead(a.Path); err != nil {
		return "", fmt.Errorf("edit needs read access as well: %w", err)
	}
	text, exists, err := s.content(resolved)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%s does not exist; use write to create it", paths.Tilde(resolved, s.o.Home))
	}
	n := strings.Count(text, a.OldString)
	replacements := 1
	if a.ReplaceAll {
		replacements = n
	}
	// Check the size before building the new text, so one call cannot make
	// autobot allocate more memory than the machine has.
	if size := len(text) + replacements*(len(a.NewString)-len(a.OldString)); n > 0 && size > maxFileBytes {
		return "", fmt.Errorf("the edited file would be %d bytes, larger than the %d byte limit", size, maxFileBytes)
	}
	switch {
	case n == 0:
		return "", fmt.Errorf("old_string was not found in %s", paths.Tilde(resolved, s.o.Home))
	case n > 1 && !a.ReplaceAll:
		return "", fmt.Errorf("old_string appears %d times in %s; include more surrounding text or set replace_all", n, paths.Tilde(resolved, s.o.Home))
	}
	var updated string
	if a.ReplaceAll {
		updated = strings.ReplaceAll(text, a.OldString, a.NewString)
	} else {
		updated = strings.Replace(text, a.OldString, a.NewString, 1)
	}
	if err := s.store(resolved, updated, "", true, false); err != nil {
		return "", err
	}
	replaced := 1
	if a.ReplaceAll {
		replaced = n
	}
	return fmt.Sprintf("Edited %s (%d replacement(s))", paths.Tilde(resolved, s.o.Home), replaced), nil
}

// store writes full to disk, or to the overlay in a dry run. In append mode
// only chunk is written to disk, so concurrent appends are not lost.
func (s *Set) store(resolved, full, chunk string, existed, appendMode bool) error {
	dir := filepath.Dir(resolved)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) && !s.overlayDir(dir) {
		if !s.o.Policy.CanCreateDir(dir) {
			return fmt.Errorf("folder %s does not exist and cannot be created; missing folders are only created inside the output folder or a folder listed under `write:`", paths.Tilde(dir, s.o.Home))
		}
	}
	if s.o.DryRun {
		if _, seen := s.original[resolved]; !seen {
			if existed {
				orig, _, err := s.content(resolved)
				if err != nil {
					orig = fmt.Sprintf("(old content not shown: %v)\n", err)
				}
				s.original[resolved] = &orig
			} else {
				s.original[resolved] = nil
			}
			s.touched = append(s.touched, resolved)
		}
		s.overlay[resolved] = full
		return nil
	}
	if isDirErr(resolved) {
		return fmt.Errorf("%s is a folder", paths.Tilde(resolved, s.o.Home))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var err error
	if appendMode {
		var f *os.File
		f, err = os.OpenFile(resolved, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err == nil {
			_, err = f.WriteString(chunk)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	} else {
		err = os.WriteFile(resolved, []byte(full), 0o644)
	}
	if err != nil {
		return err
	}
	s.written[resolved] = true
	return nil
}
