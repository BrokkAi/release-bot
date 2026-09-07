package releasebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BrokkAi/release-bot/acp"
	"github.com/BrokkAi/release-bot/internal/osrun"
)

type Agent interface {
	Execute(context.Context, string) (Result, error)
}
type agentProcess struct {
	config Config
	log    *slog.Logger
}

func (a agentProcess) Execute(ctx context.Context, prompt string) (result Result, runErr error) {
	dir := filepath.Join(a.config.StateDirectory, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return result, err
	}
	transcript, err := os.CreateTemp(dir, "session-*.jsonl")
	if err != nil {
		return result, err
	}
	defer transcript.Close()
	host, err := newHost(ctx, a.config.Directory, transcript, a.log)
	if err != nil {
		return result, err
	}
	defer host.close()
	a.log.Info("Starting agent", "command", strings.Join(a.config.Agent.Command, " "), "transcript", transcript.Name())
	cmd := osrun.StartCommand(context.Background(), a.config.Directory, a.config.Agent.Command, a.config.Agent.Environment)
	diagnostics := &osrun.Tail{Capacity: 64 << 10}
	cmd.Stderr = diagnostics
	in, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	defer out.Close()
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("launch ACP agent: %w", err)
	}
	defer func() {
		_ = osrun.Kill(cmd)
		_ = cmd.Wait()
		text, _ := diagnostics.Text()
		_ = host.record(map[string]string{"stderr": text})
		if runErr != nil && text != "" {
			runErr = fmt.Errorf("%w\nAgent diagnostics: %s", runErr, text)
		}
	}()
	connection := acp.Connect(out, in, host.request, host.notification)
	defer connection.Close()
	caps := acp.Capabilities{Terminal: true}
	caps.FS.Read = true
	caps.FS.Write = true
	init, err := connection.Initialize(ctx, caps)
	if err != nil {
		return result, err
	}
	if a.config.Agent.AuthMethod != "" {
		if err := connection.Authenticate(ctx, init, a.config.Agent.AuthMethod); err != nil {
			return result, err
		}
	}
	session, err := connection.NewSession(ctx, a.config.Directory)
	if err != nil {
		return result, fmt.Errorf("create ACP session (check agent login): %w", err)
	}
	host.mu.Lock()
	host.session = session.ID
	host.mu.Unlock()
	if a.config.Agent.Mode != "" {
		if err := connection.SetMode(ctx, &session, a.config.Agent.Mode); err != nil {
			return result, err
		}
	}
	if a.config.Agent.Model != "" {
		if err := connection.SetModel(ctx, &session, a.config.Agent.Model); err != nil {
			return result, err
		}
		a.log.Info("Using model", "model", a.config.Agent.Model)
	}
	a.log.Info("agent session", "id", session.ID, "transcript", transcript.Name())
	if err := host.record(map[string]string{"prompt": prompt}); err != nil {
		return result, err
	}
	reason, err := connection.Prompt(ctx, session, prompt)
	if err != nil {
		return result, err
	}
	if reason != "end_turn" {
		return result, fmt.Errorf("agent stopped with %s", reason)
	}
	host.mu.Lock()
	logErr := host.logError
	host.mu.Unlock()
	if logErr != nil {
		return result, logErr
	}
	text, _ := host.answer.Text()
	return parseResult(text)
}

type workspaceHost struct {
	ctx        context.Context
	cancel     context.CancelFunc
	root       *os.Root
	directory  string
	mu         sync.Mutex
	session    string
	log        *slog.Logger
	transcript *json.Encoder
	logError   error
	answer     osrun.Tail
	terminals  map[string]*commandTerminal
	next       uint64
	closing    bool
}

func newHost(ctx context.Context, dir string, output io.Writer, log *slog.Logger) (*workspaceHost, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	return &workspaceHost{ctx: ctx, cancel: cancel, root: root, directory: dir, log: log, transcript: json.NewEncoder(output), answer: osrun.Tail{Capacity: 2 << 20}, terminals: make(map[string]*commandTerminal)}, nil
}
func (h *workspaceHost) record(value any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.logError == nil {
		h.logError = h.transcript.Encode(value)
	}
	return h.logError
}
func (h *workspaceHost) notification(method string, raw json.RawMessage) error {
	if method != "session/update" {
		return nil
	}
	var update acp.Update
	if err := json.Unmarshal(raw, &update); err != nil {
		return err
	}
	h.mu.Lock()
	session := h.session
	h.mu.Unlock()
	if session != "" && update.SessionID != session {
		return nil
	}
	if err := h.record(json.RawMessage(raw)); err != nil {
		return err
	}
	if update.Update.Kind == "agent_message_chunk" {
		var content acp.Content
		if err := json.Unmarshal(update.Update.Content, &content); err != nil {
			return err
		}
		if content.Type == "text" {
			_, _ = h.answer.Write([]byte(content.Text))
		}
	}
	if update.Update.Kind == "tool_call" {
		h.log.Info("agent action", "title", update.Update.Title)
	}
	return nil
}
func (h *workspaceHost) request(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
	}
	h.mu.Lock()
	valid := h.session != "" && h.session == session.ID
	h.mu.Unlock()
	if !valid {
		return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
	}
	if method == "session/request_permission" {
		return h.permission(ctx, raw)
	}
	if err := h.ctx.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch method {
	case "fs/read_text_file", "fs/write_text_file":
		return h.file(method, raw)
	case "terminal/create", "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		return h.terminal(ctx, method, raw)
	default:
		return nil, &acp.RPCError{Code: -32601, Message: "unsupported method: " + method}
	}
}
func (h *workspaceHost) permission(ctx context.Context, raw json.RawMessage) (any, error) {
	cancelled := map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
	if ctx.Err() != nil || h.ctx.Err() != nil {
		return cancelled, nil
	}
	var params struct {
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	for _, kind := range []string{"allow_once", "allow_always"} {
		for _, option := range params.Options {
			if option.Kind == kind {
				if err := h.record(map[string]any{"permission_request": raw, "selected": option.ID}); err != nil {
					return nil, err
				}
				return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": option.ID}}, nil
			}
		}
	}
	return cancelled, nil
}
func (h *workspaceHost) relative(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("ACP file and terminal paths must be absolute")
	}
	rel, err := filepath.Rel(h.directory, path)
	if err != nil {
		return "", err
	}
	if rel != "." && !filepath.IsLocal(rel) {
		return "", errors.New("path lies outside the checkout")
	}
	return rel, nil
}
func (h *workspaceHost) file(method string, raw json.RawMessage) (any, error) {
	var p struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
		Line    *int    `json:"line"`
		Limit   *int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	path, err := h.relative(p.Path)
	if err != nil {
		return nil, err
	}
	if method == "fs/write_text_file" {
		if p.Content == nil {
			return nil, &acp.RPCError{Code: -32602, Message: "content is required"}
		}
		if err := h.root.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		return nil, h.root.WriteFile(path, []byte(*p.Content), 0644)
	}
	f, err := h.root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 4<<20 {
		return nil, errors.New("file exceeds 4 MiB; inspect it through a terminal")
	}
	lines := strings.Split(string(b), "\n")
	first, last := 0, len(lines)
	if p.Line != nil {
		if *p.Line < 1 {
			return nil, errors.New("line must be at least 1")
		}
		first = min(last, *p.Line-1)
	}
	if p.Limit != nil {
		if *p.Limit < 0 {
			return nil, errors.New("limit cannot be negative")
		}
		last = first + min(last-first, *p.Limit)
	}
	return map[string]string{"content": strings.Join(lines[first:last], "\n")}, nil
}
