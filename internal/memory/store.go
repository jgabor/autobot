package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// SharedName is the file name of shared memory, without .md.
const SharedName = "shared"

// Store is the memory folder.
type Store struct {
	Dir      string
	MaxBytes int
}

func (s *Store) file(name string) string { return filepath.Join(s.Dir, name+".md") }

func (s *Store) load(name, title, prefix string) (*Doc, error) {
	data, err := os.ReadFile(s.file(name))
	if errors.Is(err, fs.ErrNotExist) {
		return New(title, prefix), nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(string(data), title, prefix), nil
}

// Load returns a task's memory and the shared memory. Missing files are empty.
func (s *Store) Load(taskID string) (task, shared *Doc, err error) {
	if task, err = s.load(taskID, "Memory: "+taskID, "m"); err != nil {
		return nil, nil, err
	}
	if shared, err = s.load(SharedName, "Shared memory", "s"); err != nil {
		return nil, nil, err
	}
	return task, shared, nil
}

// ErrNoChange means the changes left both files as they were.
var ErrNoChange = errors.New("the changes leave memory as it was")

func (s *Store) promptFile(taskID string) string {
	return filepath.Join(s.Dir, taskID+".prompt")
}

// PromptHash returns the hash of the task prompt that the task's memory was
// last reviewed against, or "" if none is recorded.
func (s *Store) PromptHash(taskID string) (string, error) {
	data, err := os.ReadFile(s.promptFile(taskID))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

// Change applies ops for a task, enforces the size limit, writes the files
// that changed and commits. On any error the files are left as they were.
func (s *Store) Change(taskID string, ops []Op, message string) error {
	_, err := s.Review(taskID, ops, "", message)
	return err
}

// Review is Change that also records promptHash as the task prompt the
// memory was reviewed against, in the same commit, so undo reverts both.
// An empty promptHash leaves the record as it was. It reports whether the
// memory files changed, since a new prompt alone is still committed.
func (s *Store) Review(taskID string, ops []Op, promptHash, message string) (bool, error) {
	task, shared, err := s.Load(taskID)
	if err != nil {
		return false, err
	}
	oldHash, err := s.PromptHash(taskID)
	if err != nil {
		return false, err
	}
	newPrompt := promptHash != "" && promptHash != oldHash
	nt, ns, err := Apply(task, shared, ops)
	if err != nil {
		return false, err
	}
	type file struct{ name, before, after string }
	var changed []file
	for _, f := range []file{{taskID, task.Render(), nt.Render()}, {SharedName, shared.Render(), ns.Render()}} {
		if f.before == f.after {
			continue
		}
		// A file already over the limit, for example after a large import or
		// a hand edit, may still shrink or stay the same size.
		if s.MaxBytes > 0 && len(f.after) > s.MaxBytes && len(f.after) > len(f.before) {
			return false, fmt.Errorf("%s.md would be %d bytes, over the %d byte limit; remove or shorten items", f.name, len(f.after), s.MaxBytes)
		}
		changed = append(changed, f)
	}
	if len(changed) == 0 && !newPrompt {
		return false, ErrNoChange
	}
	repo, err := s.open()
	if err != nil {
		return false, err
	}
	// Commit hand edits first, so undo never reverts them along with a reflection.
	first := manualEdit
	if _, err := repo.Head(); err != nil {
		first = "start memory history"
	}
	if err := s.commit(repo, first); err != nil {
		return false, err
	}
	for _, f := range changed {
		if err := writeAtomic(s.file(f.name), f.after); err != nil {
			return false, err
		}
	}
	if newPrompt {
		if err := writeAtomic(s.promptFile(taskID), promptHash+"\n"); err != nil {
			return false, err
		}
	}
	if err := s.commit(repo, message); err != nil {
		return false, err
	}
	return len(changed) > 0, nil
}

const manualEdit = "manual edit"

func writeAtomic(path, text string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".memory-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// open opens the memory repository, creating the folder and repository as needed.
func (s *Store) open() (*git.Repository, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	repo, err := git.PlainOpen(s.Dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = git.PlainInit(s.Dir, false)
		if err == nil {
			err = os.WriteFile(filepath.Join(s.Dir, ".gitignore"), []byte(".memory-*\n"), 0o644)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("memory repository: %w", err)
	}
	return repo, nil
}

func signature() *object.Signature {
	return &object.Signature{Name: "autobot", Email: "autobot@localhost", When: time.Now()}
}

// commit records every change in the folder. It does nothing if nothing changed.
func (s *Store) commit(repo *git.Repository, message string) error {
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	status, err := wt.Status()
	if err != nil {
		return err
	}
	if status.IsClean() {
		return nil
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return err
	}
	_, err = wt.Commit(message, &git.CommitOptions{Author: signature()})
	return err
}

// Dirty reports whether memory files have changes that are not committed.
func (s *Store) Dirty() (bool, error) {
	repo, err := s.open()
	if err != nil {
		return false, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return false, err
	}
	status, err := wt.Status()
	if err != nil {
		return false, err
	}
	return !status.IsClean(), nil
}

// Commit records hand edits.
func (s *Store) Commit(message string) error {
	repo, err := s.open()
	if err != nil {
		return err
	}
	return s.commit(repo, message)
}

// ErrNoHistory means there is nothing to show or undo.
var ErrNoHistory = errors.New("memory has no recorded changes yet")

// Diff returns the last n commits as unified diffs, newest first.
func (s *Store) Diff(n int) (string, error) {
	repo, err := s.open()
	if err != nil {
		return "", err
	}
	head, err := repo.Head()
	if err != nil {
		return "", ErrNoHistory
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i := 0; i < n && commit != nil; i++ {
		var parent *object.Commit
		if commit.NumParents() > 0 {
			if parent, err = commit.Parent(0); err != nil {
				return "", err
			}
		}
		patch, err := patchBetween(parent, commit)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "commit %s\nDate: %s\n\n    %s\n\n%s\n", commit.Hash.String()[:10],
			commit.Author.When.Format(time.RFC1123), strings.TrimSpace(commit.Message), patch)
		commit = parent
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

func patchBetween(parent, commit *object.Commit) (string, error) {
	to, err := commit.Tree()
	if err != nil {
		return "", err
	}
	var from *object.Tree
	if parent != nil {
		if from, err = parent.Tree(); err != nil {
			return "", err
		}
	}
	changes, err := object.DiffTreeWithOptions(context.Background(), from, to, object.DefaultDiffTreeOptions)
	if err != nil {
		return "", err
	}
	patch, err := changes.Patch()
	if err != nil {
		return "", err
	}
	return patch.String(), nil
}

// ErrDirty means undo would discard hand edits.
var ErrDirty = errors.New("memory files have edits that are not committed; undo would discard them")

// Undo reverts the last commit by restoring its parent's files and
// committing the result. With force, uncommitted edits are discarded too.
// It returns the message of the commit it reverted.
func (s *Store) Undo(force bool) (string, error) {
	repo, err := s.open()
	if err != nil {
		return "", err
	}
	head, err := repo.Head()
	if err != nil {
		return "", ErrNoHistory
	}
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return "", err
	}
	// Step back past earlier undos and the changes they reverted, so each
	// undo goes one change further back.
	undone := map[string]bool{}
	commit := headCommit
	for {
		msg := strings.TrimSpace(commit.Message)
		if h, ok := strings.CutPrefix(lastLine(msg), revertsPrefix); ok && strings.HasPrefix(msg, "undo: ") {
			undone[h] = true
		} else if !undone[commit.Hash.String()] {
			break
		}
		if commit.NumParents() == 0 {
			return "", errors.New("every recorded change has already been undone")
		}
		if commit, err = commit.Parent(0); err != nil {
			return "", err
		}
	}
	if commit.NumParents() == 0 {
		return "", errors.New("the last change is the first one recorded; there is nothing before it to go back to")
	}
	if msg := strings.TrimSpace(commit.Message); msg == manualEdit || msg == "start memory history" {
		return "", errors.New("the last recorded change is your own edit, not a reflection; edit the memory files directly to change it")
	}
	if commit.TreeHash != headCommit.TreeHash {
		return "", errors.New("memory changed after that change in a way undo cannot separate; edit the memory files directly")
	}
	if dirty, err := s.Dirty(); err != nil {
		return "", err
	} else if dirty && !force {
		return "", ErrDirty
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return "", err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return "", err
	}
	headTree, err := commit.Tree()
	if err != nil {
		return "", err
	}
	// Remove files the reverted commit added, then restore the parent's files.
	err = headTree.Files().ForEach(func(f *object.File) error {
		if _, err := parentTree.File(f.Name); errors.Is(err, object.ErrFileNotFound) {
			if err := os.Remove(filepath.Join(s.Dir, f.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	err = parentTree.Files().ForEach(func(f *object.File) error {
		text, err := f.Contents()
		if err != nil {
			return err
		}
		return writeAtomic(filepath.Join(s.Dir, f.Name), text)
	})
	if err != nil {
		return "", err
	}
	if force {
		// Discard uncommitted files that the parent does not have either.
		entries, _ := os.ReadDir(s.Dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if _, err := parentTree.File(e.Name()); errors.Is(err, object.ErrFileNotFound) {
				os.Remove(filepath.Join(s.Dir, e.Name()))
			}
		}
	}
	msg := strings.TrimSpace(commit.Message)
	if err := s.commit(repo, "undo: "+msg+"\n\n"+revertsPrefix+commit.Hash.String()); err != nil {
		return "", err
	}
	return msg, nil
}

const revertsPrefix = "Reverts "

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Write replaces one memory file with d and commits it. It does not enforce
// the size limit, so imported memory is never cut.
func (s *Store) Write(name string, d *Doc, message string) error {
	repo, err := s.open()
	if err != nil {
		return err
	}
	if err := writeAtomic(s.file(name), d.Render()); err != nil {
		return err
	}
	return s.commit(repo, message)
}
