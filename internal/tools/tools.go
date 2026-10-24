// Package tools implements the functions a task's model can call. Every file
// operation goes through the task's access policy.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jgabor/autobot/internal/access"
	"github.com/jgabor/autobot/internal/provider"
)

// Options configure a task's tool set.
type Options struct {
	Policy         *access.Policy
	Home           string
	Commands       []string
	CommandTimeout time.Duration
	Network        bool
	// Searcher backs web_search. Nil means search is not configured.
	Searcher Searcher
	// HTTPClient backs web_fetch. Nil means a client that refuses private
	// and local addresses.
	HTTPClient *http.Client
	MaxResults int
	// DryRun keeps writes in memory so they can be shown as diffs.
	DryRun bool
}

// Set is one task's tools and the record of what they wrote.
type Set struct {
	o Options
	// Dry-run state: current content and original content by resolved path.
	overlay  map[string]string
	original map[string]*string
	touched  []string
	written  map[string]bool
	moves    []Move
}

// New builds a tool set.
func New(o Options) *Set {
	if o.MaxResults <= 0 {
		o.MaxResults = 500
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = 30 * time.Second
	}
	if o.HTTPClient == nil {
		o.HTTPClient = SafeHTTPClient(30 * time.Second)
	}
	return &Set{
		o:        o,
		overlay:  map[string]string{},
		original: map[string]*string{},
		written:  map[string]bool{},
	}
}

// Change is one file a dry run would have written.
type Change struct {
	Path    string
	Before  string
	After   string
	Existed bool
}

// Changes lists dry-run writes in the order files were first touched.
func (s *Set) Changes() []Change {
	out := make([]Change, 0, len(s.touched))
	for _, p := range s.touched {
		c := Change{Path: p, After: s.overlay[p]}
		if orig := s.original[p]; orig != nil {
			c.Before, c.Existed = *orig, true
		}
		out = append(out, c)
	}
	return out
}

// Written lists files the tools wrote, sorted.
func (s *Set) Written() []string {
	out := make([]string, 0, len(s.written))
	for p := range s.written {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func object(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func tool(name, desc string, params map[string]any) provider.Tool {
	return provider.Tool{Type: "function", Function: provider.ToolFunction{Name: name, Description: desc, Parameters: params}}
}

// Definitions returns the tools this task may use. Tools that need a
// permission the task lacks are left out.
func (s *Set) Definitions() []provider.Tool {
	defs := []provider.Tool{
		tool("read", "Read lines from a text file. Returns up to `limit` lines starting after `offset` lines.", object(map[string]any{
			"path":   str("File path. Relative paths are inside the task's output folder."),
			"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Lines to skip. Default 0."},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 2000, "description": "Lines to return. Default 200."},
		}, "path")),
		tool("list", "List files and folders matching a glob pattern. Supports *, ?, [abc] and ** for any number of folders. Folders end with /. Does not support {a,b}; make separate calls.", object(map[string]any{
			"pattern": str("Glob pattern, such as *.md, **/*.txt or ~/Documents/Notes/*.md."),
			"path":    str("Folder the pattern is relative to. Default: the output folder. Ignored when the pattern is absolute."),
		}, "pattern")),
		tool("grep", "Search file contents with a regular expression (RE2 syntax). Returns matching lines as path:line: text.", object(map[string]any{
			"pattern":     str("Regular expression."),
			"path":        str("File or folder to search. Default: the output folder."),
			"include":     str("Only search files whose name matches this glob, such as *.md."),
			"ignore_case": map[string]any{"type": "boolean", "description": "Match without regard to case."},
		}, "pattern")),
		tool("write", "Write text to a file, replacing it, or append to it. Creates missing folders inside allowed write paths.", object(map[string]any{
			"path":    str("File path. Relative paths are inside the task's output folder."),
			"content": str("Text to write."),
			"mode":    map[string]any{"type": "string", "enum": []string{"replace", "append"}, "description": "replace (default) or append."},
		}, "path", "content")),
		tool("edit", "Replace an exact piece of text in a file. old_string must appear exactly once unless replace_all is true.", object(map[string]any{
			"path":        str("File path."),
			"old_string":  str("Exact text to replace, including whitespace."),
			"new_string":  str("Replacement text."),
			"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence."},
		}, "path", "old_string", "new_string")),
		tool("move", "Move or rename a file or folder. Both paths must be writable. Never overwrites: an existing destination is an error. To put a file or folder inside a folder under its own name, give into; the folder is created if missing. To rename, give to with the full new path; if to is an existing folder or ends in /, the item goes inside it instead. With merge and to, a folder's entries are moved into another existing folder, then the empty source folder is removed; if any name already exists, nothing is moved.", object(map[string]any{
			"from":  str("File or folder to move."),
			"into":  str("Folder to move from into, keeping its name. Use instead of to."),
			"to":    str("Full new path, for a rename or a merge."),
			"merge": map[string]any{"type": "boolean", "description": "Merge the folder from into the existing folder to, instead of moving it inside."},
		}, "from")),
	}
	if len(s.o.Commands) > 0 {
		defs = append(defs, tool("run_command",
			"Run one of the allowed commands and return its output. Only these exact commands are allowed:\n- "+strings.Join(s.o.Commands, "\n- "),
			object(map[string]any{"command": str("One allowed command, exactly as listed.")}, "command")))
	}
	if s.o.Network {
		defs = append(defs,
			tool("web_search", "Search the web. Returns titles, URLs and snippets.", object(map[string]any{
				"query": str("Search query."),
			}, "query")),
			tool("web_fetch", "Fetch a public web page and return its text.", object(map[string]any{
				"url": str("http or https URL."),
			}, "url")),
		)
	}
	return defs
}

// Call runs one tool. The error text is meant for the model.
func (s *Set) Call(ctx context.Context, name, arguments string) (out string, err error) {
	// A bug in one tool must not take down the run and the tasks after it.
	defer func() {
		if r := recover(); r != nil {
			out, err = "", fmt.Errorf("tool %s failed unexpectedly: %v", name, r)
		}
	}()
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	decode := func(v any) error {
		dec := json.NewDecoder(strings.NewReader(arguments))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return fmt.Errorf("invalid arguments for %s: %w", name, err)
		}
		return nil
	}
	switch name {
	case "read":
		var a readArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.read(a)
	case "list":
		var a listArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.list(ctx, a)
	case "grep":
		var a grepArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.grep(ctx, a)
	case "write":
		var a writeArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.write(a)
	case "edit":
		var a editArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.edit(a)
	case "move":
		var a moveArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.move(a)
	case "run_command":
		if len(s.o.Commands) == 0 {
			break
		}
		var a commandArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.runCommand(ctx, a)
	case "web_search":
		if !s.o.Network {
			break
		}
		var a searchArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.webSearch(ctx, a)
	case "web_fetch":
		if !s.o.Network {
			break
		}
		var a fetchArgs
		if err := decode(&a); err != nil {
			return "", err
		}
		return s.webFetch(ctx, a)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}
