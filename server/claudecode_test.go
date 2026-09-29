package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------ fake CLI

// fakeCLI writes a shell script that stands in for "claude": it records its
// arguments, stdin and environment next to itself, prints out and exits
// with code.
func fakeCLI(t *testing.T, out string, code int) (*cliModel, string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "out"), []byte(out), 0o600)
	script := fmt.Sprintf(`#!/bin/sh
d=$(dirname "$0")
for a in "$@"; do printf '%%s\0' "$a"; done > "$d/args"
cat > "$d/stdin"
env > "$d/env"
cat "$d/out"
exit %d
`, code)
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := newCLIModel(bin, "claude-opus-5", "low", 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(m.dir) })
	return m, dir
}

func cliArgs(t *testing.T, dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

func argAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func jsonl(events ...map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		j, _ := json.Marshal(e)
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

var cliInit = map[string]any{"type": "system", "subtype": "init", "session_id": "s"}

func cliText(text string) map[string]any {
	return map[string]any{"type": "assistant", "parent_tool_use_id": nil,
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
}

func cliAPIError(category, text string) map[string]any {
	e := cliText(text)
	e["error"] = category
	return e
}

func cliResult(isError bool, result, stop string, status any, in, out int64) map[string]any {
	r := map[string]any{"type": "result", "subtype": "success", "is_error": isError, "result": result,
		"stop_reason": stop, "api_error_status": status, "num_turns": 1,
		"usage": map[string]any{"input_tokens": in, "output_tokens": out}, "modelUsage": map[string]any{}}
	if in+out > 0 {
		r["modelUsage"] = map[string]any{"claude-opus-5": map[string]any{"inputTokens": in, "outputTokens": out,
			"cacheReadInputTokens": 5, "webSearchRequests": 0}}
	}
	return r
}

func TestCLIRequestShape(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-must-not-reach-the-cli")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	m, dir := fakeCLI(t, jsonl(cliInit, cliText("Ankara."), cliResult(false, "Ankara.", "end_turn", nil, 12, 34)), 0)
	m.env = cliEnv(m.timeout) // after Setenv
	photo := []byte{0xff, 0xd8, 0xff, 0xd9}
	hist := []turn{{role: "user", content: "a", imageID: "img-old"}, {role: "assistant", content: "b"},
		{role: "user", content: "c", imageID: "img-1", image: photo}, {role: "assistant", content: "d"}}
	r, err := m.reply(context.Background(), hist, "/login e", replyOpts{instructions: "Short answers."})
	if err != nil || r.text != "Ankara." || r.inputTokens != 17 || r.outputTokens != 34 || r.cutOff || r.refused || r.mock {
		t.Fatalf("%v %+v", err, r)
	}

	args := cliArgs(t, dir)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p", "--safe-mode", "--strict-mcp-config", "--disable-slash-commands",
		"--no-session-persistence"} {
		if !strings.Contains(" "+joined+" ", " "+want+" ") {
			t.Fatalf("missing %s in %q", want, joined)
		}
	}
	for flag, want := range map[string]string{"--model": "claude-opus-5", "--effort": "low", "--tools": "",
		"--max-turns": "1", "--permission-mode": "dontAsk", "--permission-prompts": "none",
		"--input-format": "stream-json", "--output-format": "stream-json"} {
		if v, ok := argAfter(args, flag); !ok || v != want {
			t.Fatalf("%s = %q, want %q", flag, v, want)
		}
	}
	if _, ok := argAfter(args, "--allowedTools"); ok {
		t.Fatal("tools allowed without search")
	}
	sys, _ := argAfter(args, "--system-prompt")
	if !strings.Contains(sys, "Claude S40") || !strings.Contains(sys, "[New message]") ||
		!strings.Contains(sys, "Short answers.") || strings.Contains(sys, "search the web") {
		t.Fatalf("system prompt %q", sys)
	}

	// environment: only what the CLI needs; never the API key or base URL
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	for _, want := range []string{"CLAUDE_CODE_MAX_RETRIES=0", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS=8192"} {
		if !strings.Contains(string(env), want) {
			t.Fatalf("env misses %s", want)
		}
	}
	if strings.Contains(string(env), "ANTHROPIC_") || strings.Contains(string(env), "sk-ant") {
		t.Fatalf("API settings reached the CLI:\n%s", env)
	}

	// stdin: one user message; fixed text first, history, photos, then the message as is
	in, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if bytes.Count(bytes.TrimSpace(in), []byte("\n")) != 0 {
		t.Fatalf("more than one line: %s", in)
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string            `json:"type"`
				Text   string            `json:"text"`
				Source map[string]string `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(in, &msg); err != nil || msg.Type != "user" || msg.Message.Role != "user" {
		t.Fatalf("%v %s", err, in)
	}
	var kinds []string
	for _, c := range msg.Message.Content {
		kinds = append(kinds, c.Type)
	}
	if strings.Join(kinds, ",") != "text,image,text,text" {
		t.Fatalf("blocks %v", kinds)
	}
	c := msg.Message.Content
	if !strings.HasPrefix(c[0].Text, "[Conversation so far]\n\nUser: [an earlier photo, no longer shown]\na\n\nClaude: b") ||
		c[1].Source["media_type"] != "image/jpeg" || c[1].Source["data"] != "/9j/2Q==" ||
		!strings.HasPrefix(c[2].Text, "c\n\nClaude: d\n\n[New message]") || c[3].Text != "/login e" {
		t.Fatalf("content %+v", c)
	}
}

func TestCLINoHistoryWithPhoto(t *testing.T) {
	m, dir := fakeCLI(t, jsonl(cliInit, cliResult(false, "A cat.", "end_turn", nil, 1, 1)), 0)
	if _, err := m.reply(context.Background(), nil, "What is this?", replyOpts{image: []byte{1, 2}, imageID: "img-1"}); err != nil {
		t.Fatal(err)
	}
	in, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	var msg struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	json.Unmarshal(in, &msg)
	c := msg.Message.Content
	if len(c) != 3 || c[0]["text"] != "[New message]" || c[1]["type"] != "image" || c[2]["text"] != "What is this?" {
		t.Fatalf("%v", c)
	}
}

func TestCLIWebSearch(t *testing.T) {
	search := map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "Let me look that up."},
		map[string]any{"type": "tool_use", "id": "t1", "name": "WebSearch", "input": map[string]any{"query": "ankara hava"}}}}}
	sub := cliText("subagent text")
	sub["parent_tool_use_id"] = "t1"
	res := cliResult(false, "Ankara'da bugün 21 derece.", "end_turn", nil, 100, 20)
	res["modelUsage"].(map[string]any)["claude-haiku-4-5"] = map[string]any{"inputTokens": 50, "outputTokens": 5, "webSearchRequests": 2}
	m, dir := fakeCLI(t, jsonl(cliInit, search, sub, cliText("Ankara'da bugün 21 derece."), res), 0)
	r, err := m.reply(context.Background(), nil, "Ankara hava?", replyOpts{search: true, calendar: true})
	if err != nil || r.text != "Ankara'da bugün 21 derece." || r.searches != 2 || r.inputTokens != 155 || r.outputTokens != 25 {
		t.Fatalf("%v %+v", err, r)
	}
	args := cliArgs(t, dir)
	if v, _ := argAfter(args, "--tools"); v != "WebSearch" {
		t.Fatalf("tools %q", v)
	}
	if v, _ := argAfter(args, "--allowedTools"); v != "WebSearch" {
		t.Fatalf("allowed %q", v)
	}
	if v, _ := argAfter(args, "--max-turns"); v != "4" {
		t.Fatalf("max-turns %q", v)
	}
	if sys, _ := argAfter(args, "--system-prompt"); !strings.Contains(sys, "search the web") || !strings.Contains(sys, "EVENT:") {
		t.Fatal("search / calendar prompt")
	}

	// the search limit ended the run: what came after the last search, cut off
	maxTurns := map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "num_turns": 4,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 3}}
	m2, _ := fakeCLI(t, jsonl(cliInit, search, cliText("Partial"), maxTurns), 1)
	r2, err := m2.reply(context.Background(), nil, "x", replyOpts{search: true})
	if err != nil || !r2.cutOff || r2.text != "Partial" || r2.searches != 1 || r2.outputTokens != 3 {
		t.Fatalf("%v %+v", err, r2)
	}
}

func TestCLIStopReasons(t *testing.T) {
	m, _ := fakeCLI(t, jsonl(cliInit, cliText("yarım"), cliResult(false, "yarım", "max_tokens", nil, 1, 2)), 0)
	if r, err := m.reply(context.Background(), nil, "c", replyOpts{}); err != nil || !r.cutOff || r.text != "yarım" {
		t.Fatalf("%v %+v", err, r)
	}
	// the CLI's own continuation ran out: the parts written so far, cut off
	long := cliResult(true, "API Error: Claude's response exceeded the 8192 output token maximum.", "stop_sequence", nil, 48, 136)
	m2, _ := fakeCLI(t, jsonl(cliInit, cliText("one "), cliText("two"),
		cliAPIError("max_output_tokens", "API Error: Claude's response exceeded the 8192 output token maximum."), long), 1)
	if r, err := m2.reply(context.Background(), nil, "c", replyOpts{}); err != nil || !r.cutOff || r.text != "one two" || r.outputTokens != 136 {
		t.Fatalf("%v %+v", err, r)
	}
	refusal := cliResult(true, "API Error: safeguards flagged this message", "refusal", nil, 24, 68)
	m3, _ := fakeCLI(t, jsonl(cliInit, cliAPIError("invalid_request", "API Error: safeguards"), refusal), 1)
	if r, err := m3.reply(context.Background(), nil, "c", replyOpts{}); err != nil || !r.refused || r.text != "" || r.outputTokens != 68 {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestCLIErrorsNoRetry(t *testing.T) {
	cases := []struct {
		name     string
		out      string
		code     int
		kind     string
		wantCode string
	}{
		{"401", jsonl(cliInit, cliAPIError("authentication_failed", "Failed to authenticate. API Error: 401"),
			cliResult(true, "Failed to authenticate. API Error: 401", "stop_sequence", 401, 0, 0)), 1, "definite", "config_error"},
		{"not logged in", jsonl(cliInit, cliResult(true, "Not logged in · Please run /login", "", nil, 0, 0)), 1, "definite", "config_error"},
		{"429", jsonl(cliInit, cliAPIError("rate_limit", "API Error: Request rejected (429)"),
			cliResult(true, "API Error: Request rejected (429)", "stop_sequence", 429, 0, 0)), 1, "definite", "rate_limited"},
		{"usage limit", jsonl(cliInit, map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected"}},
			cliResult(true, "You've hit your limit", "", nil, 0, 0)), 1, "definite", "rate_limited"},
		{"529", jsonl(cliInit, cliResult(true, "API Error: 529", "stop_sequence", 529, 0, 0)), 1, "definite", "overloaded"},
		{"500", jsonl(cliInit, cliResult(true, "API Error: 500", "stop_sequence", 500, 0, 0)), 1, "definite", "upstream_error"},
		{"402", jsonl(cliInit, cliResult(true, "API Error: 402", "stop_sequence", 402, 0, 0)), 1, "definite", "billing"},
		{"connection", jsonl(cliInit, cliAPIError("server_error", "API Error: Connection refused"),
			cliResult(true, "API Error: Connection refused", "stop_sequence", nil, 0, 0)), 1, "uncertain", "uncertain"},
		{"crash after start", jsonl(cliInit), 3, "uncertain", "uncertain"},
		{"no session", "", 1, "definite", "config_error"},
	}
	for _, c := range cases {
		m, dir := fakeCLI(t, c.out, c.code)
		_, err := m.reply(context.Background(), nil, "c", replyOpts{})
		ue, ok := err.(*upstreamError)
		if !ok || ue.kind != c.kind || ue.code != c.wantCode {
			t.Fatalf("%s: %v", c.name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "args")); err != nil {
			t.Fatalf("%s: CLI not run", c.name)
		}
	}

	// missing binary: definite, nothing was sent
	m, _ := fakeCLI(t, "", 0)
	m.bin = filepath.Join(t.TempDir(), "no-such-claude")
	if _, err := m.reply(context.Background(), nil, "c", replyOpts{}); err == nil || err.(*upstreamError).code != "config_error" {
		t.Fatalf("missing binary: %v", err)
	}
}

func TestCLITimeoutIsUncertain(t *testing.T) {
	m, dir := fakeCLI(t, "", 0)
	os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\ncat >/dev/null\necho '{\"type\":\"system\",\"subtype\":\"init\"}'\nexec sleep 30\n"), 0o700)
	m.timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := m.reply(context.Background(), nil, "c", replyOpts{})
	if ue, ok := err.(*upstreamError); !ok || ue.kind != "uncertain" {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("not killed")
	}

	// all slots busy until the deadline: definite, the CLI was never started
	m2, dir2 := fakeCLI(t, "", 0)
	m2.timeout = 100 * time.Millisecond
	for range cliMaxProcs {
		m2.procs <- struct{}{}
	}
	if _, err := m2.reply(context.Background(), nil, "c", replyOpts{}); err == nil || err.(*upstreamError).code != "overloaded" {
		t.Fatalf("queue: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "args")); err == nil {
		t.Fatal("CLI started")
	}
}

// the chat rules hold with this backend too: one run, the reply stored and replayed
func TestCLIThroughChat(t *testing.T) {
	m, dir := fakeCLI(t, jsonl(cliInit, cliResult(false, "Merhaba.", "end_turn", nil, 3, 4)), 0)
	e := newEnv(t, 10)
	e.srv.chat.model = m
	tok, _ := e.pair("p")
	req := rid()
	r := e.chat(tok, req, "", "Selam")
	if r.msg.get("status") != "ok" || r.msg.text != "Merhaba." || r.msg.get("mock") != "0" {
		t.Fatalf("%q", r.raw)
	}
	os.Remove(filepath.Join(dir, "args"))
	if r2 := e.chat(tok, req, "", "Selam"); r2.msg.get("status") != "ok" || r2.msg.text != "Merhaba." {
		t.Fatalf("%q", r2.raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "args")); err == nil {
		t.Fatal("replay ran the CLI again")
	}
}

// ------------------------------------------------------------ the real CLI, fake Anthropic

// TestCLIReal runs the real Claude Code CLI against a local fake Messages
// API (never Anthropic): S40_CLAUDE_CLI=$(command -v claude) go test -run CLIReal
func TestCLIReal(t *testing.T) {
	bin := os.Getenv("S40_CLAUDE_CLI")
	if bin == "" {
		t.Skip("set S40_CLAUDE_CLI to a claude binary to run")
	}
	f := &fakeStream{}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	m, err := newCLIModel(bin, "claude-opus-5", "low", 3)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	// test only: the fake API instead of a subscription login; no proxies
	m.env = append(cliEnv(m.timeout)[:7], "HOME="+home, "PATH=/usr/bin:/bin",
		"ANTHROPIC_BASE_URL="+ts.URL, "ANTHROPIC_API_KEY=sk-ant-test-not-a-real-key")

	var photo bytes.Buffer
	jpeg.Encode(&photo, image.NewGray(image.Rect(0, 0, 16, 12)), nil)
	f.set(200, "Ankara.", "end_turn")
	r, err := m.reply(context.Background(), []turn{{role: "user", content: "a"}, {role: "assistant", content: "b"}},
		"Başkent?", replyOpts{image: photo.Bytes()})
	if err != nil || r.text != "Ankara." || r.outputTokens == 0 || r.cutOff {
		t.Fatalf("%v %+v", err, r)
	}
	if n := f.count(); n != 1 {
		t.Fatalf("%d model calls", n)
	}
	body := f.last()
	raw, _ := json.Marshal(body)
	if body["model"] != "claude-opus-5" || len(body["tools"].([]any)) != 0 ||
		!strings.Contains(string(raw), "Claude S40") || !strings.Contains(string(raw), "Başkent?") ||
		!strings.Contains(string(raw), `"media_type":"image/jpeg"`) {
		t.Fatalf("request %s", truncate(string(raw), 2000))
	}

	for _, c := range []struct {
		status int
		code   string
	}{{401, "config_error"}, {429, "rate_limited"}, {529, "overloaded"}, {500, "upstream_error"}} {
		f.set(c.status, "", "")
		before := f.count()
		_, err := m.reply(context.Background(), nil, "c", replyOpts{})
		if ue, ok := err.(*upstreamError); !ok || ue.code != c.code || ue.kind != "definite" {
			t.Fatalf("%d: %v", c.status, err)
		}
		if n := f.count() - before; n != 1 {
			t.Fatalf("%d: %d calls (retried)", c.status, n)
		}
	}

	// the CLI itself asks once more after a refusal (2.1.284); never more
	f.set(200, "", "refusal")
	before := f.count()
	if r, err := m.reply(context.Background(), nil, "c", replyOpts{}); err != nil || !r.refused {
		t.Fatalf("refusal: %v %+v", err, r)
	}
	if n := f.count() - before; n < 1 || n > 2 {
		t.Fatalf("refusal: %d calls", n)
	}
}

// fakeStream: a Messages API that answers every call with one streamed text reply or an error.
type fakeStream struct {
	mu     sync.Mutex
	status int
	text   string
	stop   string
	calls  int
	bodies []map[string]any
}

func (f *fakeStream) set(status int, text, stop string) {
	f.mu.Lock()
	f.status, f.text, f.stop = status, text, stop
	f.mu.Unlock()
}

func (f *fakeStream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeStream) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") || strings.Contains(r.URL.Path, "count_tokens") {
		http.NotFound(w, r)
		return
	}
	var b map[string]any
	json.NewDecoder(r.Body).Decode(&b)
	f.mu.Lock()
	f.calls++
	f.bodies = append(f.bodies, b)
	status, text, stop := f.status, f.text, f.stop
	f.mu.Unlock()
	if status != 200 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, errBody("api_error", fmt.Sprintf("fake %d", status)))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(typ string, data map[string]any) {
		j, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, j)
	}
	ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "type": "message",
		"role": "assistant", "model": b["model"], "content": []any{}, "stop_reason": nil,
		"usage": map[string]any{"input_tokens": 12, "output_tokens": 1}}})
	ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text}})
	ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop},
		"usage": map[string]any{"output_tokens": 34}})
	ev("message_stop", map[string]any{"type": "message_stop"})
}
