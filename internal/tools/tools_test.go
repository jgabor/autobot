package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jgabor/autobot/internal/access"
)

type env struct {
	home   string
	output string
	set    *Set
}

func write(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, read, writeRoots []string, mod func(*Options)) env {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	output := filepath.Join(home, ".local/state/autobot/inbox/task")
	os.MkdirAll(output, 0o755)
	write(t, filepath.Join(home, "notes/a.md"), "alpha\nbeta\ngamma\n")
	write(t, filepath.Join(home, "notes/b.txt"), "Beta in caps\n")
	write(t, filepath.Join(home, "notes/sub/c.md"), "deep beta\n")
	write(t, filepath.Join(home, "notes/bin.dat"), "beta\x00binary")
	write(t, filepath.Join(home, "secret/s.md"), "beta secret\n")
	policy, err := access.New(access.Options{
		Home: home, Output: output, Read: read, Write: writeRoots,
		Protected: []string{filepath.Join(home, ".local/state/autobot")},
	})
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Policy: policy, Home: home, MaxResults: 100}
	if mod != nil {
		mod(&o)
	}
	return env{home, output, New(o)}
}

func call(t *testing.T, s *Set, name string, args map[string]any) (string, error) {
	t.Helper()
	data, _ := json.Marshal(args)
	return s.Call(context.Background(), name, string(data))
}

func mustCall(t *testing.T, s *Set, name string, args map[string]any) string {
	t.Helper()
	out, err := call(t, s, name, args)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return out
}

func TestRead(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, nil, nil)
	if out := mustCall(t, e.set, "read", map[string]any{"path": "~/notes/a.md"}); out != "alpha\nbeta\ngamma\n" {
		t.Errorf("read: %q", out)
	}
	out := mustCall(t, e.set, "read", map[string]any{"path": "~/notes/a.md", "offset": 1, "limit": 1})
	if !strings.HasPrefix(out, "beta\n") || !strings.Contains(out, "lines 2-2 of 3") || !strings.Contains(out, "offset=2") {
		t.Errorf("paged read: %q", out)
	}
	for _, bad := range []map[string]any{
		{"path": "~/secret/s.md"},
		{"path": "~/notes/missing.md"},
		{"path": "~/notes"},
		{"path": "~/notes/bin.dat"},
		{"path": "~/notes/a.md", "offset": 10},
		{"path": "~/notes/a.md", "limit": 5000},
		{"path": "~/notes/a.md", "bogus": true},
	} {
		if _, err := call(t, e.set, "read", bad); err == nil {
			t.Errorf("read %v: want error", bad)
		}
	}
}

func TestList(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, nil, nil)
	out := mustCall(t, e.set, "list", map[string]any{"pattern": "~/notes/*.md"})
	if out != "~/notes/a.md" {
		t.Errorf("*.md: %q", out)
	}
	out = mustCall(t, e.set, "list", map[string]any{"pattern": "**/*.md", "path": "~/notes"})
	if out != "~/notes/a.md\n~/notes/sub/c.md" {
		t.Errorf("**/*.md: %q", out)
	}
	out = mustCall(t, e.set, "list", map[string]any{"pattern": "~/notes/*"})
	if !strings.Contains(out, "~/notes/sub/") || strings.Contains(out, "c.md") {
		t.Errorf("* must list one level and mark folders: %q", out)
	}
	out = mustCall(t, e.set, "list", map[string]any{"pattern": "*.nothing", "path": "~/notes"})
	if !strings.HasPrefix(out, "No matches") {
		t.Errorf("no matches: %q", out)
	}
	if _, err := call(t, e.set, "list", map[string]any{"pattern": "~/**/*.md"}); err == nil {
		t.Error("listing home without read access must be denied")
	}
	if _, err := call(t, e.set, "list", map[string]any{"pattern": "~/notes/*.{md,txt}"}); err == nil {
		t.Error("brace patterns must be rejected")
	}
}

func TestListLimit(t *testing.T) {
	e := newEnv(t, []string{"~/many/"}, nil, func(o *Options) { o.MaxResults = 3 })
	for i := range 10 {
		write(t, filepath.Join(e.home, fmt.Sprintf("many/%02d.md", i)), "x")
	}
	out := mustCall(t, e.set, "list", map[string]any{"pattern": "~/many/*.md"})
	if strings.Count(out, ".md") != 3 || !strings.Contains(out, "limited to 3") {
		t.Errorf("limit: %q", out)
	}
}

func TestListSkipsProtected(t *testing.T) {
	e := newEnv(t, []string{"~"}, nil, nil)
	write(t, filepath.Join(e.home, ".local/state/autobot/memory/shared.md"), "secret memory")
	out := mustCall(t, e.set, "list", map[string]any{"pattern": "~/**/*.md"})
	if strings.Contains(out, "memory/shared.md") {
		t.Errorf("protected file listed: %q", out)
	}
	out = mustCall(t, e.set, "grep", map[string]any{"pattern": "secret memory", "path": "~"})
	if out != "No matches found." {
		t.Errorf("protected file searched: %q", out)
	}
}

func TestGrep(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, nil, nil)
	out := mustCall(t, e.set, "grep", map[string]any{"pattern": "beta", "path": "~/notes"})
	if !strings.Contains(out, "~/notes/a.md:2: beta") || !strings.Contains(out, "~/notes/sub/c.md:1: deep beta") {
		t.Errorf("grep: %q", out)
	}
	if strings.Contains(out, "bin.dat") || strings.Contains(out, "secret") || strings.Contains(out, "Beta in caps") {
		t.Errorf("grep matched binary, protected or wrong case: %q", out)
	}
	out = mustCall(t, e.set, "grep", map[string]any{"pattern": "beta", "path": "~/notes", "ignore_case": true, "include": "*.txt"})
	if out != "~/notes/b.txt:1: Beta in caps" {
		t.Errorf("grep include and ignore_case: %q", out)
	}
	out = mustCall(t, e.set, "grep", map[string]any{"pattern": "zzz", "path": "~/notes/a.md"})
	if out != "No matches found." {
		t.Errorf("no match: %q", out)
	}
	if _, err := call(t, e.set, "grep", map[string]any{"pattern": "(", "path": "~/notes"}); err == nil {
		t.Error("invalid regexp: want error")
	}
	if _, err := call(t, e.set, "grep", map[string]any{"pattern": "beta", "path": "~/secret"}); err == nil {
		t.Error("grep outside read paths: want error")
	}
}

func TestWriteAndEdit(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, []string{"~/notes/next.md", "~/reports/"}, nil)
	mustCall(t, e.set, "write", map[string]any{"path": "~/notes/next.md", "content": "one\n"})
	mustCall(t, e.set, "write", map[string]any{"path": "~/notes/next.md", "content": "two\n", "mode": "append"})
	data, _ := os.ReadFile(filepath.Join(e.home, "notes/next.md"))
	if string(data) != "one\ntwo\n" {
		t.Errorf("write then append: %q", data)
	}
	mustCall(t, e.set, "write", map[string]any{"path": "~/reports/2026/october.md", "content": "x"})
	if _, err := os.Stat(filepath.Join(e.home, "reports/2026/october.md")); err != nil {
		t.Errorf("missing folders inside a write root must be created: %v", err)
	}
	mustCall(t, e.set, "write", map[string]any{"path": "summary.md", "content": "in output"})
	if _, err := os.Stat(filepath.Join(e.output, "summary.md")); err != nil {
		t.Errorf("relative write goes to the output folder: %v", err)
	}
	if _, err := call(t, e.set, "write", map[string]any{"path": "~/notes/a.md", "content": "x"}); err == nil {
		t.Error("write to a read-only path: want error")
	}

	out := mustCall(t, e.set, "edit", map[string]any{"path": "~/notes/next.md", "old_string": "two", "new_string": "three"})
	if !strings.Contains(out, "Edited") {
		t.Errorf("edit: %q", out)
	}
	data, _ = os.ReadFile(filepath.Join(e.home, "notes/next.md"))
	if string(data) != "one\nthree\n" {
		t.Errorf("after edit: %q", data)
	}
	mustCall(t, e.set, "write", map[string]any{"path": "~/notes/next.md", "content": "x x x"})
	if _, err := call(t, e.set, "edit", map[string]any{"path": "~/notes/next.md", "old_string": "x", "new_string": "y"}); err == nil || !strings.Contains(err.Error(), "3 times") {
		t.Errorf("ambiguous edit: %v", err)
	}
	mustCall(t, e.set, "edit", map[string]any{"path": "~/notes/next.md", "old_string": "x", "new_string": "y", "replace_all": true})
	data, _ = os.ReadFile(filepath.Join(e.home, "notes/next.md"))
	if string(data) != "y y y" {
		t.Errorf("replace_all: %q", data)
	}
	if _, err := call(t, e.set, "edit", map[string]any{"path": "~/notes/next.md", "old_string": "nope", "new_string": "y"}); err == nil {
		t.Error("edit with missing old_string: want error")
	}
	if _, err := call(t, e.set, "edit", map[string]any{"path": "~/reports/new.md", "old_string": "a", "new_string": "b"}); err == nil {
		t.Error("edit of a missing file: want error")
	}
	written := e.set.Written()
	if len(written) != 3 {
		t.Errorf("Written: %v", written)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, []string{"~/notes"}, func(o *Options) { o.DryRun = true })
	mustCall(t, e.set, "write", map[string]any{"path": "~/notes/new.md", "content": "hello\n"})
	mustCall(t, e.set, "edit", map[string]any{"path": "~/notes/a.md", "old_string": "beta", "new_string": "BETA"})
	mustCall(t, e.set, "write", map[string]any{"path": "~/notes/new.md", "content": "again\n", "mode": "append"})
	if _, err := os.Stat(filepath.Join(e.home, "notes/new.md")); err == nil {
		t.Fatal("dry run created a file")
	}
	data, _ := os.ReadFile(filepath.Join(e.home, "notes/a.md"))
	if string(data) != "alpha\nbeta\ngamma\n" {
		t.Fatalf("dry run changed a file: %q", data)
	}
	// Later reads see the dry-run content.
	if out := mustCall(t, e.set, "read", map[string]any{"path": "~/notes/new.md"}); out != "hello\nagain\n" {
		t.Errorf("read after dry-run write: %q", out)
	}
	changes := e.set.Changes()
	if len(changes) != 2 {
		t.Fatalf("changes: %+v", changes)
	}
	if changes[0].Existed || changes[0].After != "hello\nagain\n" {
		t.Errorf("new file change: %+v", changes[0])
	}
	if !changes[1].Existed || changes[1].Before != "alpha\nbeta\ngamma\n" || changes[1].After != "alpha\nBETA\ngamma\n" {
		t.Errorf("edit change: %+v", changes[1])
	}
	if len(e.set.Written()) != 0 {
		t.Error("dry run must not report written files")
	}
}

func TestRunCommand(t *testing.T) {
	e := newEnv(t, nil, nil, func(o *Options) {
		o.Commands = []string{"echo hello world", "false", "sleep 5"}
		o.CommandTimeout = 200 * time.Millisecond
	})
	if out := mustCall(t, e.set, "run_command", map[string]any{"command": " echo hello world "}); out != "hello world\n" {
		t.Errorf("echo: %q", out)
	}
	if out := mustCall(t, e.set, "run_command", map[string]any{"command": "false"}); !strings.Contains(out, "exit status 1") {
		t.Errorf("false: %q", out)
	}
	if _, err := call(t, e.set, "run_command", map[string]any{"command": "echo hello"}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("unlisted command: %v", err)
	}
	if _, err := call(t, e.set, "run_command", map[string]any{"command": "sleep 5"}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout: %v", err)
	}
}

func TestToolsNeedPermission(t *testing.T) {
	e := newEnv(t, nil, nil, nil)
	names := map[string]bool{}
	for _, d := range e.set.Definitions() {
		names[d.Function.Name] = true
	}
	for _, n := range []string{"run_command", "web_search", "web_fetch"} {
		if names[n] {
			t.Errorf("%s offered without permission", n)
		}
		if _, err := call(t, e.set, n, map[string]any{}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("%s callable without permission: %v", n, err)
		}
	}
	e = newEnv(t, nil, nil, func(o *Options) { o.Commands = []string{"true"}; o.Network = true })
	names = map[string]bool{}
	for _, d := range e.set.Definitions() {
		names[d.Function.Name] = true
	}
	for _, n := range []string{"read", "list", "grep", "write", "edit", "run_command", "web_search", "web_fetch"} {
		if !names[n] {
			t.Errorf("%s not offered", n)
		}
	}
}

func TestWebFetchRefusesLocalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "local secret") }))
	defer srv.Close()
	e := newEnv(t, nil, nil, func(o *Options) { o.Network = true })
	_, err := call(t, e.set, "web_fetch", map[string]any{"url": srv.URL})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("fetch of a loopback address: %v", err)
	}
	if _, err := call(t, e.set, "web_fetch", map[string]any{"url": "file:///etc/passwd"}); err == nil {
		t.Fatal("file URL: want error")
	}
}

func TestWebFetchExtractsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<html><head><title>Title</title><style>.x{}</style></head><body><script>var a=1;</script><h1>Hello</h1><p>First   para &amp; more.</p><p>Second</p></body></html>`)
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		}
	}))
	defer srv.Close()
	e := newEnv(t, nil, nil, func(o *Options) { o.Network = true; o.HTTPClient = srv.Client() })
	out := mustCall(t, e.set, "web_fetch", map[string]any{"url": srv.URL + "/page"})
	for _, want := range []string{"Status: 200", "Title", "Hello", "First para & more.", "Second"} {
		if !strings.Contains(out, want) {
			t.Errorf("fetch output missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "var a") || strings.Contains(out, ".x{}") {
		t.Errorf("script or style leaked: %q", out)
	}
	if _, err := call(t, e.set, "web_fetch", map[string]any{"url": srv.URL + "/image"}); err == nil {
		t.Error("image: want unsupported content type")
	}
}

func TestWebSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/search" || r.Header.Get("Authorization") != "Bearer key123" || body.Query != "go testing" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"data":null,"errors":[{"code":"general.invalid_token","message":"bad token"}]}`)
			return
		}
		fmt.Fprint(w, `{"meta":{},"data":{"search":[{"url":"https://go.dev","title":"Go","snippet":"The Go language"}],"related_search":[{"url":"x","title":"related"}]}}`)
	}))
	defer srv.Close()
	e := newEnv(t, nil, nil, func(o *Options) {
		o.Network = true
		o.Searcher = &Kagi{Key: "key123", BaseURL: srv.URL, Client: srv.Client()}
	})
	out := mustCall(t, e.set, "web_search", map[string]any{"query": "go testing"})
	if out != "1. Go\n   https://go.dev\n   The Go language" {
		t.Errorf("search: %q", out)
	}
	e = newEnv(t, nil, nil, func(o *Options) {
		o.Network = true
		o.Searcher = &Kagi{Key: "wrong", BaseURL: srv.URL, Client: srv.Client()}
	})
	if _, err := call(t, e.set, "web_search", map[string]any{"query": "go testing"}); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("search error: %v", err)
	}
	e = newEnv(t, nil, nil, func(o *Options) { o.Network = true })
	if _, err := call(t, e.set, "web_search", map[string]any{"query": "x"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("unconfigured search: %v", err)
	}
}

func TestMatchSegments(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"*.md", "a.md", true},
		{"*.md", "sub/a.md", false},
		{"**/*.md", "a.md", true},
		{"**/*.md", "x/y/a.md", true},
		{"a/**", "a/b/c", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/b/b/c", true},
		{"?.md", "ab.md", false},
	}
	for _, c := range cases {
		if got := matchSegments(strings.Split(c.pat, "/"), strings.Split(c.name, "/")); got != c.want {
			t.Errorf("match(%q, %q) = %v", c.pat, c.name, got)
		}
	}
}

func TestCommandTimeoutKillsChildren(t *testing.T) {
	e := newEnv(t, nil, nil, func(o *Options) {
		o.Commands = []string{"sh -c 'sleep 5 & sleep 5; echo done'"}
		o.CommandTimeout = 200 * time.Millisecond
	})
	start := time.Now()
	_, err := call(t, e.set, "run_command", map[string]any{"command": "sh -c 'sleep 5 & sleep 5; echo done'"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout, got %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the call took %s; child processes kept it open", took)
	}
}

func TestCommandsRunInAnEmptyFolder(t *testing.T) {
	e := newEnv(t, nil, nil, func(o *Options) { o.Commands = []string{"ls -A"} })
	write(t, filepath.Join(e.output, ".git/config"), "[core]\n\tfsmonitor = touch /tmp/pwned\n")
	if out := mustCall(t, e.set, "run_command", map[string]any{"command": "ls -A"}); out != "(no output)" {
		t.Fatalf("commands must not start in the writable output folder: %q", out)
	}
}

func TestReadRefusesPipes(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, nil, nil)
	fifo := filepath.Join(e.home, "notes/pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := call(t, e.set, "read", map[string]any{"path": "~/notes/pipe"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("read of a pipe: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read of a pipe blocked")
	}
	if out := mustCall(t, e.set, "grep", map[string]any{"pattern": "x", "path": "~/notes/pipe"}); out != "No matches found." {
		t.Fatalf("grep of a pipe: %q", out)
	}
}

func TestWalkStopsWhenCancelled(t *testing.T) {
	e := newEnv(t, []string{"~/many/"}, nil, nil)
	for i := range 600 {
		write(t, filepath.Join(e.home, fmt.Sprintf("many/%03d.txt", i)), "needle")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := e.set.Call(ctx, "grep", `{"pattern":"needle","path":"~/many"}`)
	if err != nil || !strings.Contains(out, "cancelled") {
		t.Fatalf("grep with a cancelled context: %q %v", out, err)
	}
}

func TestDryRunFilesAreListedAndSearched(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, []string{"~/notes"}, func(o *Options) { o.DryRun = true })
	mustCall(t, e.set, "write", map[string]any{"path": "~/notes/new.md", "content": "fresh needle\n"})
	if out := mustCall(t, e.set, "list", map[string]any{"pattern": "~/notes/*.md"}); !strings.Contains(out, "~/notes/new.md") {
		t.Errorf("list misses the dry-run file: %q", out)
	}
	if out := mustCall(t, e.set, "grep", map[string]any{"pattern": "needle", "path": "~/notes"}); !strings.Contains(out, "~/notes/new.md:1: fresh needle") {
		t.Errorf("grep misses the dry-run file: %q", out)
	}
}

func TestReplaceBinaryFile(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, []string{"~/notes"}, nil)
	mustCall(t, e.set, "write", map[string]any{"path": "~/notes/bin.dat", "content": "text now"})
	if _, err := call(t, e.set, "write", map[string]any{"path": "~/notes/sub", "content": "x"}); err == nil {
		t.Error("writing over a folder: want error")
	}
}

func TestPublicIP(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "169.254.1.1", "100.64.0.1", "198.18.0.1", "::1", "fd00::1", "64:ff9b::a00:1", "2002:a00:1::1", "0.0.0.1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Errorf("%s counted as public", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(ip)) {
			t.Errorf("%s counted as private", ip)
		}
	}
}

func TestFileRootCannotBecomeAFolder(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, []string{"~/notes/next-actions.md", "~/new-file.md"}, nil)
	for _, p := range []string{"~/notes/next-actions.md/a/b/evil.sh", "~/new-file.md/x"} {
		if _, err := call(t, e.set, "write", map[string]any{"path": p, "content": "x"}); err == nil {
			t.Errorf("write %s: want denied", p)
		}
	}
	mustCall(t, e.set, "write", map[string]any{"path": "~/notes/next-actions.md", "content": "ok"})
	mustCall(t, e.set, "write", map[string]any{"path": "~/new-file.md", "content": "ok"})
}

func TestMissingFoldersOnlyInFolderRoots(t *testing.T) {
	e := newEnv(t, nil, []string{"~/missing/next.md"}, func(o *Options) { o.DryRun = true })
	if _, err := call(t, e.set, "write", map[string]any{"path": "~/missing/next.md", "content": "x"}); err == nil || !strings.Contains(err.Error(), "cannot be created") {
		t.Fatalf("a dry run must refuse what a real run refuses: %v", err)
	}
}

func TestEditNeedsReadAccess(t *testing.T) {
	e := newEnv(t, nil, []string{"~/secret/s.md"}, nil)
	if _, err := call(t, e.set, "edit", map[string]any{"path": "~/secret/s.md", "old_string": "beta", "new_string": "x"}); err == nil || !strings.Contains(err.Error(), "read access") {
		t.Fatalf("edit of a write-only file: %v", err)
	}
	mustCall(t, e.set, "write", map[string]any{"path": "~/secret/s.md", "content": "replaced"})
}

func TestEditCannotBlowUpMemory(t *testing.T) {
	e := newEnv(t, nil, nil, nil)
	mustCall(t, e.set, "write", map[string]any{"path": "x.txt", "content": strings.Repeat("a", 20000)})
	_, err := call(t, e.set, "edit", map[string]any{"path": "x.txt", "old_string": "a", "new_string": strings.Repeat("b", 2000), "replace_all": true})
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("huge edit: %v", err)
	}
	if _, err := call(t, e.set, "write", map[string]any{"path": "y.txt", "content": strings.Repeat("c", maxFileBytes+1)}); err == nil {
		t.Fatal("oversized write: want error")
	}
}

func TestManyDoubleStarsStayFast(t *testing.T) {
	name := strings.Split("a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/x", "/")
	pat := strings.Split(strings.Repeat("**/", 30)+"y", "/")
	start := time.Now()
	if matchSegments(pat, name) {
		t.Fatal("unexpected match")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("matching took %s", time.Since(start))
	}
}

func TestComplexRegexpRefused(t *testing.T) {
	e := newEnv(t, []string{"~/notes"}, nil, nil)
	if _, err := call(t, e.set, "grep", map[string]any{"pattern": "(?:[a-z]{1,100}){1,10}y", "path": "~/notes"}); err == nil || !strings.Contains(err.Error(), "too complex") {
		t.Fatalf("complex pattern: %v", err)
	}
	mustCall(t, e.set, "grep", map[string]any{"pattern": `^\s*- \[ \] .*(milk|bread)`, "path": "~/notes"})
}

func TestFolderRootIsNotWrittenAsAFile(t *testing.T) {
	e := newEnv(t, nil, []string{"~/reports/", "~/deep/er/"}, nil)
	if _, err := call(t, e.set, "write", map[string]any{"path": "~/reports", "content": "x"}); err == nil {
		t.Fatal("writing a folder root as a file: want error")
	}
	mustCall(t, e.set, "write", map[string]any{"path": "~/reports/a.md", "content": "x"})
	if _, err := call(t, e.set, "write", map[string]any{"path": "~/deep/er/a.md", "content": "x"}); err == nil {
		t.Fatal("parents above a declared folder must not be created")
	}
}
