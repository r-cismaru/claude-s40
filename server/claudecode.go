package main

// Claude calls through the Claude Code CLI ("claude -p"), for a server that
// uses its owner's Claude subscription (Pro/Max) instead of an API key
// (CLAUDE_BACKEND=claude-code). The CLI is Anthropic's unmodified binary and
// keeps its own login, made with "claude auth login" (Anthropic's sign-in
// flow) in its own config directory. This server never reads, stores or
// forwards that credential: it starts the CLI with a fixed, locked-down
// command line (no tools except web search, no MCP servers, no slash
// commands, no CLAUDE.md, no session files) and an environment of its own
// (never ANTHROPIC_API_KEY), sends one user message as stream-json and reads
// the answer from the stream-json output.
//
// Anthropic allows a subscription only for the account owner's own use of
// Claude Code: this backend is for a personal server that serves only its
// owner's phones, never other people (docs/SETUP.md).
//
// CLAUDE_CODE_MAX_RETRIES=0: a retried timeout could be a second call.
// Errors are classified as in claude.go: definite (the CLI never started a
// session, or Anthropic answered with an error) or uncertain.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	cliTimeout = 120 * time.Second // below pendingStale: a slow reply is still recorded
	// the CLI continues a reply that hits its output limit with more calls,
	// so the limit is higher than maxTokens; the prompt keeps answers short
	cliMaxOutputTokens = 8192
	cliMaxProcs        = 2       // CLI processes at once (each needs ~200-300 MB)
	cliMaxStdout       = 8 << 20 // stream-json output kept per call
)

// cliPromptNote: how the conversation reaches the CLI (one user message).
const cliPromptNote = `

Each request shows the conversation so far (if any) and then, after "[New message]", the user's new message. Answer only the new message and do not repeat these labels.`

type cliModel struct {
	bin     string
	model   string
	effort  string
	maxUses int64 // web searches per message
	timeout time.Duration
	dir     string   // empty working directory for the CLI
	env     []string // the CLI's whole environment
	procs   chan struct{}
	now     func() time.Time
}

func newCLIModel(bin, modelID, effort string, maxUses int64) (*cliModel, error) {
	dir, err := os.MkdirTemp("", "claude-s40-cli-")
	if err != nil {
		return nil, err
	}
	return &cliModel{bin: bin, model: modelID, effort: effort, maxUses: max(1, maxUses), timeout: cliTimeout,
		dir: dir, env: cliEnv(cliTimeout), procs: make(chan struct{}, cliMaxProcs), now: time.Now}, nil
}

// cliEnv: the CLI's whole environment. Nothing else from the server's
// environment is passed on; in particular no ANTHROPIC_API_KEY or
// ANTHROPIC_BASE_URL, so the CLI uses its own subscription login.
func cliEnv(timeout time.Duration) []string {
	env := []string{
		"CLAUDE_CODE_MAX_RETRIES=0",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", // no telemetry, error reports or auto-updates
		"CLAUDE_CODE_DISABLE_TERMINAL_TITLE=1",       // no extra request for a session title
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS=" + strconv.Itoa(cliMaxOutputTokens),
		"API_TIMEOUT_MS=" + strconv.FormatInt(timeout.Milliseconds(), 10),
	}
	for _, k := range []string{"HOME", "PATH", "TMPDIR", "CLAUDE_CONFIG_DIR", "HTTPS_PROXY", "https_proxy",
		"HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (m *cliModel) args(o replyOpts) []string {
	a := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", m.model, "--system-prompt", systemPrompt(o, m.now()) + cliPromptNote,
		"--safe-mode", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence",
		"--permission-mode", "dontAsk", "--permission-prompts", "none", "--disallowedTools", "mcp__*"}
	if m.effort != "" {
		a = append(a, "--effort", m.effort)
	}
	if o.search {
		return append(a, "--tools", "WebSearch", "--allowedTools", "WebSearch",
			"--max-turns", strconv.FormatInt(m.maxUses+1, 10))
	}
	return append(a, "--tools", "", "--max-turns", "1")
}

// cliInput: the whole conversation as one stream-json user message. It
// always starts with a fixed text block, so the user's text can never be
// read as a slash command.
func cliInput(history []turn, message string, img []byte, imageID string) []byte {
	var content []map[string]any
	var sb strings.Builder
	flush := func() {
		if sb.Len() > 0 {
			content = append(content, map[string]any{"type": "text", "text": sb.String()})
			sb.Reset()
		}
	}
	image := func(b []byte) {
		flush()
		content = append(content, map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/jpeg", "data": base64.StdEncoding.EncodeToString(b)}})
	}
	if len(history) > 0 {
		sb.WriteString("[Conversation so far]")
		for _, t := range history {
			if t.role == "assistant" {
				sb.WriteString("\n\nClaude: " + t.content)
				continue
			}
			sb.WriteString("\n\nUser:")
			switch {
			case len(t.image) > 0:
				sb.WriteString(" (with this photo)")
				image(t.image)
				sb.WriteString(t.content)
			case t.imageID != "":
				sb.WriteString(" [an earlier photo, no longer shown]\n" + t.content)
			default:
				sb.WriteString(" " + t.content)
			}
		}
		sb.WriteString("\n\n")
	}
	sb.WriteString("[New message]")
	if len(img) > 0 {
		image(img)
	}
	flush()
	content = append(content, map[string]any{"type": "text", "text": message})
	b, _ := json.Marshal(map[string]any{"type": "user", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": content}})
	return append(b, '\n')
}

// cliEvent: the parts of the CLI's stream-json lines that are used.
type cliEvent struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Error           string  `json:"error"` // on an assistant message the CLI made from an API error
	Message         *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	RateLimitInfo *struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
	// result
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	StopReason     string `json:"stop_reason"`
	APIErrorStatus *int   `json:"api_error_status"`
	Usage          struct {
		InputTokens   int64 `json:"input_tokens"`
		OutputTokens  int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		InputTokens       int64 `json:"inputTokens"`
		OutputTokens      int64 `json:"outputTokens"`
		CacheRead         int64 `json:"cacheReadInputTokens"`
		CacheCreation     int64 `json:"cacheCreationInputTokens"`
		WebSearchRequests int64 `json:"webSearchRequests"`
	} `json:"modelUsage"`
}

// cliRun: what one CLI run produced.
type cliRun struct {
	started  bool   // the CLI started a session (system/init)
	text     string // main-thread answer text after the last tool call
	errKind  string // error category of the last CLI-made error message
	limited  bool   // the subscription's usage limit rejected the request
	searches int64  // web search tool calls
	result   *cliEvent
}

func parseCLIOutput(out []byte) cliRun {
	var run cliRun
	var text strings.Builder
	for _, line := range bytes.Split(out, []byte("\n")) {
		var ev cliEvent
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				run.started = true
			}
		case "rate_limit_event":
			if ev.RateLimitInfo != nil && ev.RateLimitInfo.Status == "rejected" {
				run.limited = true
			}
		case "assistant":
			if ev.ParentToolUseID != nil || ev.Message == nil {
				continue
			}
			if ev.Error != "" { // an API error written as a message, not Claude's text
				run.errKind = ev.Error
				continue
			}
			for _, b := range ev.Message.Content {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool_use", "server_tool_use":
					// text before or between searches is narration; keep
					// only the answer after the last one
					text.Reset()
					if b.Name == "WebSearch" || b.Name == "web_search" {
						run.searches++
					}
				}
			}
		case "result":
			r := ev
			run.result = &r
		}
	}
	run.text = text.String()
	return run
}

func (m *cliModel) reply(ctx context.Context, history []turn, message string, o replyOpts) (reply, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	select {
	case m.procs <- struct{}{}:
		defer func() { <-m.procs }()
	case <-ctx.Done():
		// nothing was sent: the other CLI runs took all the time
		logJSON(map[string]any{"evt": "upstream_error", "backend": "claude-code", "kind": "queue_timeout"})
		return reply{}, &upstreamError{"definite", "overloaded"}
	}

	cmd := exec.CommandContext(ctx, m.bin, m.args(o)...)
	cmd.Dir, cmd.Env = m.dir, m.env
	cmd.Stdin = bytes.NewReader(cliInput(history, message, o.image, o.imageID))
	var stdout, stderr capWriter
	stdout.max, stderr.max = cliMaxStdout, 2048
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	run := parseCLIOutput(stdout.buf.Bytes())
	return m.interpret(run, runErr, ctx.Err(), stderr.buf.String())
}

// interpret maps one CLI run to a reply or an upstream error and logs the
// class of a failure (status, category, exit code; never content).
func (m *cliModel) interpret(run cliRun, runErr, ctxErr error, stderr string) (reply, error) {
	fail := func(kind, code string, extra map[string]any) (reply, error) {
		ev := map[string]any{"evt": "upstream_error", "backend": "claude-code", "code": code}
		for k, v := range extra {
			ev[k] = v
		}
		logJSON(ev)
		return reply{}, &upstreamError{kind, code}
	}
	res := run.result
	if res == nil {
		exit := -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exit = ee.ExitCode()
		}
		if !run.started && ctxErr == nil {
			// the CLI did not get as far as a session (missing binary,
			// unknown flag, unreadable config): no call was made
			return fail("definite", "config_error", map[string]any{"exit": exit, "stderr": truncate(stderr, 200)})
		}
		kind := "transport"
		if ctxErr != nil {
			kind = "timeout"
		}
		return fail("uncertain", "uncertain", map[string]any{"kind": kind, "exit": exit})
	}

	var out reply
	if len(res.ModelUsage) > 0 {
		var ws int64
		for _, u := range res.ModelUsage {
			out.inputTokens += u.InputTokens + u.CacheRead + u.CacheCreation
			out.outputTokens += u.OutputTokens
			ws += u.WebSearchRequests
		}
		out.searches = max(run.searches, ws)
	} else {
		out.inputTokens = res.Usage.InputTokens + res.Usage.CacheRead + res.Usage.CacheCreation
		out.outputTokens = res.Usage.OutputTokens
		out.searches = run.searches
	}

	switch {
	case res.StopReason == "refusal":
		out.refused = true
		return out, nil
	case !res.IsError && res.Subtype == "success":
		out.text = res.Result
		out.cutOff = res.StopReason == "max_tokens"
		return out, nil
	case res.Subtype == "error_max_turns" || run.errKind == "max_output_tokens":
		// the answer did not finish within the search or length limit:
		// what was written counts as a reply that was cut off
		out.text, out.cutOff = run.text, true
		return out, nil
	}

	info := map[string]any{"subtype": res.Subtype, "category": run.errKind}
	if res.APIErrorStatus != nil {
		st := *res.APIErrorStatus
		info["status"] = st
		switch {
		case st == 402:
			return fail("definite", "billing", info)
		case st == 429:
			return fail("definite", "rate_limited", info)
		case st == 401 || st == 403 || st == 404:
			return fail("definite", "config_error", info)
		case st == 529 || st == 503:
			return fail("definite", "overloaded", info)
		default:
			return fail("definite", "upstream_error", info)
		}
	}
	switch {
	case run.limited || run.errKind == "rate_limit":
		return fail("definite", "rate_limited", info)
	case run.errKind == "billing_error":
		return fail("definite", "billing", info)
	case run.errKind == "overloaded":
		return fail("definite", "overloaded", info)
	case run.errKind == "authentication_failed" || run.errKind == "oauth_org_not_allowed" ||
		run.errKind == "account_on_hold" || run.errKind == "model_not_found" ||
		strings.Contains(res.Result, "/login"):
		return fail("definite", "config_error", info)
	}
	// no HTTP answer (connection error, timeout...): the call may have been processed
	return fail("uncertain", "uncertain", info)
}

// capWriter keeps at most max bytes and drops the rest.
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
