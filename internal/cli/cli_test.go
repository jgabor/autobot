package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jgabor/autobot/internal/app"
	"github.com/jgabor/autobot/internal/config"
	"github.com/jgabor/autobot/internal/memory"
	"github.com/jgabor/autobot/internal/notify"
	"github.com/jgabor/autobot/internal/paths"
	"github.com/jgabor/autobot/internal/provider"
	"github.com/jgabor/autobot/internal/record"
	"github.com/jgabor/autobot/internal/schedule"
)

type harness struct {
	t     *testing.T
	home  string
	paths paths.Paths
	out   bytes.Buffer
	err   bytes.Buffer
	now   time.Time
	fake  *provider.Fake
	sent  []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	app.LingerDir = filepath.Join(home, "linger")
	return &harness{
		t:    t,
		home: home,
		paths: paths.Paths{
			Home:       home,
			ConfigHome: filepath.Join(home, ".config"),
			ConfigDir:  filepath.Join(home, ".config/autobot"),
			StateDir:   filepath.Join(home, ".local/state/autobot"),
		},
		now:  time.Date(2026, 10, 20, 9, 15, 0, 0, time.Local),
		fake: &provider.Fake{},
	}
}

// app builds an App that uses the fake provider unless real is set.
func (h *harness) app(real bool) *app.App {
	a := app.New(h.paths, &h.out, &h.err)
	a.Now = func() time.Time { return h.now }
	a.Executable = "/usr/local/bin/autobot"
	a.Env = func(string) string { return "" }
	if !real {
		a.NewProvider = func(config.Provider, string) provider.Provider { return h.fake }
	}
	a.NewNotifier = func(c config.Notify) notify.Notifier {
		if c.Method == "none" {
			return nil
		}
		return notifierFunc(func(title, body string) { h.sent = append(h.sent, title+"\n"+body) })
	}
	return a
}

type notifierFunc func(title, body string)

func (f notifierFunc) Send(_ context.Context, title, body string) error { f(title, body); return nil }

func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	return Run(context.Background(), h.app(false), args[0], args[1:])
}

func (h *harness) runReal(args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	return Run(context.Background(), h.app(true), args[0], args[1:])
}

func (h *harness) write(rel, content string) string {
	h.t.Helper()
	p := filepath.Join(h.home, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) read(rel string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.home, rel))
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func (h *harness) config(extra string) {
	h.write(".config/autobot/config.toml", "[provider]\nurl = \"http://127.0.0.1:1/v1\"\nmodel = \"test-model\"\n"+extra)
}

func (h *harness) expect(code, want int) {
	h.t.Helper()
	if code != want {
		h.t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, want, h.out.String(), h.err.String())
	}
}

func TestInitOnEmptyHomeAndAgain(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("init"), 0)
	for _, f := range []string{".config/autobot/config.toml", ".config/autobot/system.md", ".config/autobot/tasks/hello.md",
		".config/systemd/user/autobot.service", ".config/systemd/user/autobot.timer"} {
		if !strings.Contains(h.out.String(), "created: ~/"+f) {
			t.Errorf("init did not report %s:\n%s", f, h.out.String())
		}
	}
	if !strings.Contains(h.out.String(), "autobot check") || !strings.Contains(h.out.String(), "enable-linger") {
		t.Errorf("init must print next steps:\n%s", h.out.String())
	}
	service := h.read(".config/systemd/user/autobot.service")
	if !strings.Contains(service, "ExecStart=/usr/local/bin/autobot run --due") || !strings.Contains(service, "Type=oneshot") ||
		!strings.Contains(service, "EnvironmentFile=-"+filepath.Join(h.home, ".config/autobot/environment")) {
		t.Errorf("service unit:\n%s", service)
	}
	if !strings.Contains(h.read(".config/systemd/user/autobot.timer"), "OnCalendar=*:0/15") {
		t.Error("timer must fire every 15 minutes")
	}
	info, _ := os.Stat(filepath.Join(h.home, ".config/autobot/config.toml"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config.toml mode %v, want 0600", info.Mode().Perm())
	}

	h.write(".config/autobot/system.md", "my own prompt")
	h.expect(h.run("init"), 0)
	if strings.Count(h.out.String(), "exists, left unchanged") != 5 || strings.Contains(h.out.String(), "created:") {
		t.Errorf("second init:\n%s", h.out.String())
	}
	if h.read(".config/autobot/system.md") != "my own prompt" {
		t.Error("init overwrote system.md")
	}
}

func TestInitThenCheckOffline(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("init"), 0)
	h.expect(h.run("check", "--offline"), 0)
	for _, want := range []string{"ok    config", "ok    task hello: weekly, due now", "warn  provider: not tested", "warn  linger: off"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("check output missing %q:\n%s", want, h.out.String())
		}
	}
}

func TestNewTask(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.expect(h.run("new", "weekly-review"), 0)
	if !strings.Contains(h.out.String(), "autobot run weekly-review --dry-run") {
		t.Errorf("new output:\n%s", h.out.String())
	}
	h.expect(h.run("check", "--offline"), 0)
	if !strings.Contains(h.out.String(), "warn  task weekly-review: disabled") {
		t.Errorf("template does not parse:\n%s", h.out.String())
	}
	h.expect(h.run("new", "weekly-review"), 1)
	h.expect(h.run("new", "Bad Name"), 1)
	h.expect(h.run("new"), 2)
}

// openAIServer replies with scripted chat completions and records requests.
type openAIServer struct {
	mu       sync.Mutex
	replies  []string
	requests []map[string]any
}

func (s *openAIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req)
	s.requests = append(s.requests, req)
	if len(s.replies) == 0 {
		http.Error(w, `{"error":{"message":"no reply scripted"}}`, 500)
		return
	}
	reply := s.replies[0]
	s.replies = s.replies[1:]
	fmt.Fprintf(w, `{"choices":[{"message":%s,"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`, reply)
}

func toolCalls(calls ...[2]string) string {
	var parts []string
	for i, c := range calls {
		args, _ := json.Marshal(c[1])
		parts = append(parts, fmt.Sprintf(`{"id":"call_%d","type":"function","function":{"name":%q,"arguments":%s}}`, i, c[0], args))
	}
	return `{"role":"assistant","content":null,"tool_calls":[` + strings.Join(parts, ",") + `]}`
}

func text(s string) string {
	b, _ := json.Marshal(s)
	return `{"role":"assistant","content":` + string(b) + `}`
}

func TestRunEndToEnd(t *testing.T) {
	h := newHarness(t)
	srv := &openAIServer{replies: []string{
		toolCalls([2]string{"list", `{"pattern":"~/notes/*.md"}`}, [2]string{"read", `{"path":"~/notes/a.md"}`}),
		toolCalls([2]string{"write", `{"path":"~/notes/next.md","content":"- call the bank\n"}`}),
		text("Wrote one next action to ~/notes/next.md."),
		text(`[{"op":"add","section":"Locations","text":"Notes are in ~/notes; a.md has the todo list"}]`),
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	h.write(".config/autobot/config.toml", fmt.Sprintf("[provider]\nurl = %q\nmodel = \"test-model\"\n", ts.URL+"/v1"))
	h.write(".config/autobot/tasks/notes.md", "---\nschedule: daily\nread: [~/notes]\nwrite: [~/notes/next.md]\n---\nReview my notes and write next actions.\n")
	h.write("notes/a.md", "TODO call the bank\n")

	h.expect(h.runReal("run", "notes"), 0)

	if got := h.read("notes/next.md"); got != "- call the bank\n" {
		t.Errorf("next.md: %q", got)
	}
	outputs, _ := filepath.Glob(filepath.Join(h.paths.InboxDir(), "notes", "*.md"))
	if len(outputs) != 1 {
		t.Fatalf("inbox outputs: %v", outputs)
	}
	if data, _ := os.ReadFile(outputs[0]); !strings.Contains(string(data), "Wrote one next action") {
		t.Errorf("inbox output: %q", data)
	}
	latest := h.read(".local/state/autobot/inbox/latest.md")
	if !strings.Contains(latest, "| notes | completed |") {
		t.Errorf("digest:\n%s", latest)
	}

	// The first request carries the access rules and only the permitted tools.
	system := srv.requests[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, want := range []string{"Read access: ~/notes", "Write access: ~/notes/next.md", "Network: none", "Task: notes", "Nothing yet", "Home folder: " + h.home} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q:\n%s", want, system)
		}
	}
	var names []string
	for _, tool := range srv.requests[0]["tools"].([]any) {
		names = append(names, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "read,list,grep,write,edit,move" {
		t.Errorf("tools offered: %v", names)
	}
	// The second request includes the tool results.
	msgs := srv.requests[1]["messages"].([]any)
	if last := msgs[len(msgs)-1].(map[string]any); last["role"] != "tool" || !strings.Contains(last["content"].(string), "TODO call the bank") {
		t.Errorf("tool result not sent back: %v", last)
	}

	runs, _ := record.List(h.paths.RunsDir())
	if len(runs) != 1 {
		t.Fatalf("runs: %+v", runs)
	}
	m := runs[0]
	if m.Status != "completed" || m.Rounds != 2 || m.Tools["read"] != 1 || m.PromptTokens != 300 ||
		len(m.FilesWritten) != 1 || m.FilesWritten[0] != "~/notes/next.md" || m.Reflection != "memory: 1 added" {
		t.Errorf("run meta: %+v", m)
	}

	st, _ := schedule.LoadState(h.paths.ScheduleFile())
	if !st.Tasks["notes"].LastRun.Equal(h.now) || st.Tasks["notes"].LastStatus != "completed" {
		t.Errorf("schedule state: %+v", st)
	}
	if mem := h.read(".local/state/autobot/memory/notes.md"); !strings.Contains(mem, "- [m-1] Notes are in ~/notes") {
		t.Errorf("memory:\n%s", mem)
	}

	h.expect(h.run("log"), 0)
	if !strings.Contains(h.out.String(), m.ID) || !strings.Contains(h.out.String(), "completed") {
		t.Errorf("log:\n%s", h.out.String())
	}
	h.expect(h.run("show", "notes"), 0)
	for _, want := range []string{"Status: completed", "call list", "result of read", "TODO call the bank", "## Final answer", "system prompt ("} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("show missing %q:\n%s", want, h.out.String())
		}
	}
	h.expect(h.run("show", "--full"), 0)
	if !strings.Contains(h.out.String(), "You are autobot") {
		t.Error("show --full must include the system prompt")
	}

	// Nothing is due again until tomorrow.
	h.now = h.now.Add(15 * time.Minute)
	h.config("")
	h.expect(h.run("run", "--due"), 0)
	if !strings.Contains(h.out.String(), "Nothing is due.") {
		t.Errorf("run --due:\n%s", h.out.String())
	}

	h.expect(h.run("memory", "diff"), 0)
	if !strings.Contains(h.out.String(), "reflect: notes ("+m.ID+")") || !strings.Contains(h.out.String(), "+- [m-1] Notes are in") {
		t.Errorf("memory diff:\n%s", h.out.String())
	}
	h.expect(h.run("memory", "undo"), 0)
	if !strings.Contains(h.out.String(), "Reverted: reflect: notes") {
		t.Errorf("memory undo:\n%s", h.out.String())
	}
	if data, err := os.ReadFile(filepath.Join(h.paths.MemoryDir(), "notes.md")); err == nil && strings.Contains(string(data), "Notes are in") {
		t.Error("undo did not revert memory")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.write(".config/autobot/tasks/notes.md", "---\nwrite: [~/notes]\nread: [~/notes]\n---\nWrite next actions.\n")
	h.write("notes/next.md", "old line\n")
	h.fake.Replies = []provider.Reply{
		provider.Calls(provider.Call("c1", "write", `{"path":"~/notes/next.md","content":"new line\n"}`)),
		provider.Calls(provider.Call("c2", "write", `{"path":"~/notes/created.md","content":"fresh\n"}`)),
		provider.Text("Done."),
	}
	h.expect(h.run("run", "notes", "--dry-run"), 0)
	out := h.out.String()
	for _, want := range []string{"dry run", "--- final answer ---\nDone.", "-old line", "+new line", "--- /dev/null", "+fresh"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output missing %q:\n%s", want, out)
		}
	}
	if h.read("notes/next.md") != "old line\n" {
		t.Error("dry run changed a file")
	}
	for _, p := range []string{filepath.Join(h.home, "notes/created.md"), h.paths.StateDir} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("dry run created %s", p)
		}
	}
	if len(h.fake.Requests) != 3 {
		t.Errorf("dry run must not reflect: %d requests", len(h.fake.Requests))
	}
}

func TestMovesAreRecorded(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.write(".config/autobot/tasks/sort.md", "---\nread: [~/dl]\nwrite: [~/dl/]\n---\nSort my downloads.\n")
	h.write("dl/a.pdf", "a")
	move := provider.Calls(provider.Call("c1", "move", `{"from":"~/dl/a.pdf","to":"~/dl/Documents/"}`))

	h.fake.Replies = []provider.Reply{move, provider.Text("Sorted.")}
	h.expect(h.run("run", "sort", "--dry-run"), 0)
	if out := h.out.String(); !strings.Contains(out, "moves that would be made") || !strings.Contains(out, "~/dl/a.pdf -> ~/dl/Documents/a.pdf") {
		t.Errorf("dry run output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.home, "dl/a.pdf")); err != nil {
		t.Fatal("dry run moved the file")
	}

	h.fake.Replies = []provider.Reply{move, provider.Text("Sorted."), provider.Text("[]")}
	h.expect(h.run("run", "sort"), 0)
	if _, err := os.Stat(filepath.Join(h.home, "dl/Documents/a.pdf")); err != nil {
		t.Fatal("file was not moved")
	}
	runs, _ := record.List(h.paths.RunsDir())
	if len(runs) != 1 || len(runs[0].FilesMoved) != 1 || runs[0].FilesMoved[0] != "~/dl/a.pdf -> ~/dl/Documents/a.pdf" {
		t.Fatalf("runs: %+v", runs)
	}
	h.expect(h.run("show", "sort"), 0)
	if out := h.out.String(); !strings.Contains(out, "- Files moved: 1") || !strings.Contains(out, "## Files moved\n\n- ~/dl/a.pdf -> ~/dl/Documents/a.pdf") {
		t.Errorf("report:\n%s", out)
	}
}

func TestDueSelectsTasksAndReportsBrokenFilesOnce(t *testing.T) {
	h := newHarness(t)
	h.config("[notify]\nmethod = \"ntfy\"\nntfy_url = \"https://ntfy.invalid/t\"\n")
	h.write(".config/autobot/tasks/hourly.md", "---\nschedule: hourly\n---\nHourly chore.\n")
	h.write(".config/autobot/tasks/off.md", "---\nenabled: false\n---\nDisabled chore.\n")
	h.write(".config/autobot/tasks/later.md", "---\nschedule: weekly\n---\nWeekly chore.\n")
	h.write(".config/autobot/tasks/broken.md", "---\nshedule: daily\n---\nTypo in header.\n")
	st := schedule.State{Tasks: map[string]schedule.TaskState{"later": {LastRun: h.now.Add(-time.Hour)}}}
	os.MkdirAll(h.paths.StateDir, 0o700)
	st.Save(h.paths.ScheduleFile())

	h.fake.Replies = []provider.Reply{provider.Text("Hourly done."), provider.Text("[]")}
	h.expect(h.run("run", "--due"), 1)
	latest := h.read(".local/state/autobot/inbox/latest.md")
	if !strings.Contains(latest, "| hourly | completed |") || !strings.Contains(latest, "| broken | invalid |") ||
		strings.Contains(latest, "| off |") || strings.Contains(latest, "| later |") || !strings.Contains(latest, "shedule") {
		t.Errorf("digest:\n%s", latest)
	}
	if len(h.sent) != 1 || !strings.Contains(h.sent[0], "1 failed") || !strings.Contains(h.sent[0], "broken: invalid") {
		t.Errorf("notifications: %q", h.sent)
	}

	h.now = h.now.Add(20 * time.Minute)
	h.expect(h.run("run", "--due"), 0)
	if !strings.Contains(h.out.String(), "Nothing is due.") || len(h.sent) != 1 {
		t.Errorf("the broken task must be reported once; output %q, notifications %d", h.out.String(), len(h.sent))
	}
}

func TestFailedTaskDoesNotStopOthers(t *testing.T) {
	h := newHarness(t)
	h.config("[notify]\nmethod = \"notify-send\"\non = \"failure\"\n")
	h.write(".config/autobot/tasks/a-first.md", "First.\n")
	h.write(".config/autobot/tasks/b-second.md", "Second.\n")
	h.fake.Replies = []provider.Reply{
		{Err: fmt.Errorf("provider returned HTTP 500: overloaded")},
		provider.Text("Second done."),
		provider.Text("[]"),
	}
	h.expect(h.run("run", "a-first", "b-second"), 1)
	latest := h.read(".local/state/autobot/inbox/latest.md")
	if !strings.Contains(latest, "| a-first | failed |") || !strings.Contains(latest, "| b-second | completed |") {
		t.Errorf("digest:\n%s", latest)
	}
	runs, _ := record.List(h.paths.RunsDir())
	for _, r := range runs {
		if r.Task == "a-first" && r.Reflection != "skipped" {
			t.Errorf("reflection must be skipped after a provider failure: %+v", r)
		}
	}
	if len(h.sent) != 1 || !strings.Contains(h.sent[0], "overloaded") {
		t.Errorf("notifications: %q", h.sent)
	}
}

func TestFailuresStillReflect(t *testing.T) {
	h := newHarness(t)
	h.config("[defaults]\nmax_rounds = 1\n")
	h.write(".config/autobot/tasks/loop.md", "Loop.\n")
	h.fake.Replies = []provider.Reply{
		provider.Calls(provider.Call("c1", "list", `{"pattern":"*"}`)),
		provider.Calls(provider.Call("c2", "list", `{"pattern":"*"}`)),
		provider.Text(`[{"op":"add","section":"Gotchas","text":"Stop listing; answer sooner"}]`),
	}
	h.expect(h.run("run", "loop"), 1)
	if !strings.Contains(h.read(".local/state/autobot/memory/loop.md"), "answer sooner") {
		t.Error("a task failure should still teach memory")
	}
	reflectPrompt := h.fake.Requests[2].Messages[1].Content
	if !strings.Contains(reflectPrompt, "Status: failed") || !strings.Contains(reflectPrompt, "maximum number of tool rounds") {
		t.Errorf("reflection prompt:\n%s", reflectPrompt)
	}
}

func TestAccessDeniedReachesTheModel(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.write(".config/autobot/tasks/snoop.md", "---\nread: [~/notes]\n---\nRead my secrets.\n")
	h.write("secret.txt", "password")
	h.fake.Replies = []provider.Reply{
		provider.Calls(provider.Call("c1", "read", `{"path":"~/secret.txt"}`), provider.Call("c2", "read", `{"path":"~/.config/autobot/config.toml"}`)),
		provider.Text("I could not read them."),
		provider.Text("[]"),
	}
	h.expect(h.run("run", "snoop"), 0)
	msgs := h.fake.Requests[1].Messages
	first, second := msgs[len(msgs)-2].Content, msgs[len(msgs)-1].Content
	if !strings.Contains(first, "access denied") || strings.Contains(first, "password") {
		t.Errorf("outside read: %q", first)
	}
	if !strings.Contains(second, "autobot's own files") {
		t.Errorf("config read: %q", second)
	}
}

func TestKeysAreRedacted(t *testing.T) {
	h := newHarness(t)
	h.write(".config/autobot/config.toml", "[provider]\nurl = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\napi_key_command = \"echo sk-very-secret\"\n")
	h.write(".config/autobot/tasks/leak.md", "Leak.\n")
	h.fake.Replies = []provider.Reply{provider.Text("The key is sk-very-secret."), provider.Text("[]")}
	h.expect(h.run("run", "leak"), 0)
	err := filepath.Walk(h.paths.StateDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(p, "/.git/") {
			return err
		}
		data, _ := os.ReadFile(p)
		if bytes.Contains(data, []byte("sk-very-secret")) {
			t.Errorf("%s contains the key", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCheckProvider(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.write(".config/autobot/tasks/a.md", "A.\n")
	h.fake.Replies = []provider.Reply{provider.Calls(provider.Call("c1", "ping", `{"message":"hello"}`))}
	h.expect(h.run("check"), 0)
	if !strings.Contains(h.out.String(), "ok    tool calls: test-model called the test tool") {
		t.Errorf("check:\n%s", h.out.String())
	}
	h.fake.Replies = []provider.Reply{provider.Text("Hello! How can I help?")}
	h.expect(h.run("check"), 1)
	if !strings.Contains(h.out.String(), "FAIL  tool calls") || !strings.Contains(h.out.String(), "How can I help") {
		t.Errorf("check without tool support:\n%s", h.out.String())
	}
	h.fake.Replies = []provider.Reply{{Err: fmt.Errorf("connection refused")}}
	h.expect(h.run("check"), 1)
	if !strings.Contains(h.out.String(), "FAIL  provider: connection refused") {
		t.Errorf("check with provider down:\n%s", h.out.String())
	}
}

func TestSetupErrorsAreVisible(t *testing.T) {
	h := newHarness(t)
	h.expect(h.run("run", "--due"), 1)
	if !strings.Contains(h.err.String(), "autobot init") {
		t.Errorf("stderr: %s", h.err.String())
	}
	if !strings.Contains(h.read(".local/state/autobot/inbox/latest.md"), "autobot init") {
		t.Error("a broken setup must show up in the inbox")
	}
	h.now = h.now.Add(15 * time.Minute)
	h.expect(h.run("run", "--due"), 1)
	if digests, _ := filepath.Glob(filepath.Join(h.paths.InboxDir(), "digests", "*.md")); len(digests) != 1 {
		t.Errorf("the same setup error must be written once, got %d digests", len(digests))
	}
	h.config("")
	h.write(".config/autobot/tasks/other.md", "Other.\n")
	h.expect(h.run("run", "missing"), 1)
	if !strings.Contains(h.err.String(), "autobot new missing") {
		t.Errorf("unknown task error: %s", h.err.String())
	}
}

func TestUsageErrors(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"run"}, {"run", "--due", "x"}, {"run", "--due", "--dry-run"}, {"bogus"}, {"memory"}, {"memory", "nope"}, {"log", "-n", "0"}, {"check", "extra"}} {
		if code := h.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	var out, errOut bytes.Buffer
	if Main(context.Background(), []string{"help"}, &out, &errOut) != 0 || !strings.Contains(out.String(), "autobot run --due") {
		t.Error("help")
	}
}

func TestVersionFromBuildInfo(t *testing.T) {
	mod := func(v string) *debug.BuildInfo { return &debug.BuildInfo{Main: debug.Module{Version: v}} }
	for _, c := range []struct {
		set  string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"dev", mod("v0.1.1"), true, "v0.1.1"},
		{"v9.9.9", mod("v0.1.1"), true, "v9.9.9"},
		{"dev", mod("(devel)"), true, "dev"},
		{"dev", mod(""), true, "dev"},
		{"dev", nil, false, "dev"},
	} {
		if got := versionFrom(c.set, c.info, c.ok); got != c.want {
			t.Errorf("versionFrom(%q, %v): got %q, want %q", c.set, c.info, got, c.want)
		}
	}
}

func TestEnvironmentFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "environment")
	os.WriteFile(p, []byte("# comment\nOPENAI_API_KEY=sk-1\nexport QUOTED=\"a b\"\nSINGLE='c'\n\nbad line\n"), 0o600)
	vars, err := app.ParseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if vars["OPENAI_API_KEY"] != "sk-1" || vars["QUOTED"] != "a b" || vars["SINGLE"] != "c" || len(vars) != 3 {
		t.Fatalf("vars: %v", vars)
	}
}

func TestProviderDownIsRetriedAndReportedOnce(t *testing.T) {
	h := newHarness(t)
	h.config("[notify]\nmethod = \"notify-send\"\n")
	h.write(".config/autobot/tasks/weekly.md", "---\nschedule: weekly\n---\nWeekly chore.\n")
	down := provider.Reply{Err: fmt.Errorf("connection refused")}
	h.fake.Replies = []provider.Reply{down, down}
	h.expect(h.run("run", "--due"), 1)
	if len(h.sent) != 1 {
		t.Fatalf("first failure must notify: %q", h.sent)
	}
	st, _ := schedule.LoadState(h.paths.ScheduleFile())
	if !st.Tasks["weekly"].LastRun.IsZero() {
		t.Fatal("a provider failure must not count as a run")
	}
	runs, _ := record.List(h.paths.RunsDir())
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("the failed attempt must leave a record: %+v", runs)
	}

	// Still down at the next check: retried, but not reported again.
	h.now = h.now.Add(15 * time.Minute)
	h.expect(h.run("run", "--due"), 1)
	if len(h.fake.Requests) != 2 || len(h.sent) != 1 {
		t.Fatalf("want a retry without a second notification: %d requests, %d notifications", len(h.fake.Requests), len(h.sent))
	}

	// Back up: the task runs and counts.
	h.now = h.now.Add(15 * time.Minute)
	h.fake.Replies = []provider.Reply{provider.Text("Done."), provider.Text("[]")}
	h.expect(h.run("run", "--due"), 0)
	st, _ = schedule.LoadState(h.paths.ScheduleFile())
	if !st.Tasks["weekly"].LastRun.Equal(h.now) || len(h.sent) != 2 {
		t.Fatalf("recovery: %+v, %d notifications", st.Tasks["weekly"], len(h.sent))
	}
}

func TestAPIKeyCommandFailureLeavesARecord(t *testing.T) {
	h := newHarness(t)
	h.write(".config/autobot/config.toml", "[provider]\nurl = \"http://127.0.0.1:1/v1\"\nmodel = \"m\"\napi_key_command = \"exit 1\"\n")
	h.write(".config/autobot/tasks/a.md", "A.\n")
	h.expect(h.run("run", "a"), 1)
	runs, _ := record.List(h.paths.RunsDir())
	if len(runs) != 1 || runs[0].Status != "failed" || !strings.Contains(runs[0].Error, "api_key_command") {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestNetworkTaskDoesNotSeeSharedMemory(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.write(".local/state/autobot/memory/shared.md", "# Shared memory\n\n## Insights\n\n- [s-1] private fact from another task\n")
	h.write(".config/autobot/tasks/web.md", "---\nnetwork: true\n---\nLook something up.\n")
	h.write(".config/autobot/tasks/local.md", "Local chore.\n")
	h.fake.Replies = []provider.Reply{provider.Text("ok"), provider.Text("[]"), provider.Text("ok"), provider.Text("[]")}
	h.expect(h.run("run", "local", "web"), 0)
	if !strings.Contains(h.fake.Requests[0].Messages[0].Content, "private fact") {
		t.Error("a local task should see shared memory")
	}
	if strings.Contains(h.fake.Requests[2].Messages[0].Content, "private fact") {
		t.Error("a network task must not see shared memory")
	}
}

func TestProviderFailureAfterToolsCountsAsARun(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.write(".config/autobot/tasks/log.md", "---\nschedule: hourly\nwrite: [~/log.md]\n---\nAppend a line.\n")
	h.fake.Replies = []provider.Reply{
		provider.Calls(provider.Call("c1", "write", `{"path":"~/log.md","content":"line\n","mode":"append"}`)),
		{Err: fmt.Errorf("provider returned HTTP 400: context too long")},
	}
	h.expect(h.run("run", "--due"), 1)
	st, _ := schedule.LoadState(h.paths.ScheduleFile())
	if !st.Tasks["log"].LastRun.Equal(h.now) {
		t.Fatal("a task that already wrote files must count as run, or the next check repeats its side effects")
	}
	h.now = h.now.Add(15 * time.Minute)
	h.expect(h.run("run", "--due"), 0)
	if h.read("log.md") != "line\n" {
		t.Fatalf("log.md: %q", h.read("log.md"))
	}
}

func TestHandEditsAreCommittedBeforeARun(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.write(".config/autobot/tasks/a.md", "A.\n")
	h.fake.Replies = []provider.Reply{provider.Text("ok"), provider.Text(`[{"op":"add","section":"Locations","text":"x"}]`)}
	h.expect(h.run("run", "a"), 0)
	mem := filepath.Join(h.paths.MemoryDir(), "a.md")
	data, _ := os.ReadFile(mem)
	os.WriteFile(mem, append(data, []byte("- by hand\n")...), 0o644)
	h.fake.Replies = []provider.Reply{provider.Text("ok"), provider.Text("[]")}
	h.expect(h.run("run", "a"), 0)
	store := &memory.Store{Dir: h.paths.MemoryDir()}
	if dirty, _ := store.Dirty(); dirty {
		t.Fatal("hand edits must be committed when a task runs")
	}
}

type panicProvider struct{}

func (panicProvider) Chat(context.Context, provider.Request) (provider.Response, error) {
	panic("boom")
}

func TestPanicInOneTaskDoesNotStopOthers(t *testing.T) {
	h := newHarness(t)
	h.config("")
	h.write(".config/autobot/tasks/a.md", "A.\n")
	a := h.app(false)
	a.NewProvider = func(config.Provider, string) provider.Provider { return panicProvider{} }
	h.out.Reset()
	h.err.Reset()
	if code := Run(context.Background(), a, "run", []string{"--due"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.read(".local/state/autobot/inbox/latest.md"), "internal error") {
		t.Fatal("the panic must be reported in the digest")
	}
	st, _ := schedule.LoadState(h.paths.ScheduleFile())
	if !st.Tasks["a"].LastRun.Equal(h.now) {
		t.Fatal("a crashing task must count as run")
	}
}
