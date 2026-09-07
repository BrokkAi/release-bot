package releasebot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This test process acts as a tiny ACP peer over real stdin/stdout pipes.
// Its wire messages are authored from the protocol, with no SDK dependency.
func TestWirePeer(t *testing.T) {
	mode := os.Getenv("RELEASE_BOT_WIRE_PEER")
	if mode == "" {
		return
	}
	read := json.NewDecoder(os.Stdin)
	write := json.NewEncoder(os.Stdout)
	request := func(method string, params any) map[string]json.RawMessage {
		if err := write.Encode(map[string]any{"jsonrpc": "2.0", "id": "peer-request", "method": method, "params": params}); err != nil {
			os.Exit(21)
		}
		var response struct {
			Result map[string]json.RawMessage
			Error  any
		}
		if err := read.Decode(&response); err != nil || response.Error != nil {
			fmt.Fprintln(os.Stderr, "callback failed", err, response.Error)
			os.Exit(22)
		}
		return response.Result
	}
	dir := ""
	for {
		var m struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		if err := read.Decode(&m); err != nil {
			os.Exit(0)
		}
		var result any = map[string]any{}
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}, "authMethods": []any{map[string]string{"id": "fixture", "name": "Fixture login"}}}
		case "authenticate", "session/set_mode":
		case "session/new":
			var p struct{ Cwd string }
			_ = json.Unmarshal(m.Params, &p)
			dir = p.Cwd
			result = map[string]string{"sessionId": "fixture-session"}
		case "session/prompt":
			if mode == "hang" {
				time.Sleep(time.Hour)
				os.Exit(23)
			}
			params := map[string]any{"sessionId": "fixture-session", "toolCall": map[string]string{"toolCallId": "build"}, "options": []any{map[string]string{"optionId": "yes", "kind": "allow_once", "name": "Allow once"}}}
			permission := request("session/request_permission", params)
			if !strings.Contains(string(permission["outcome"]), `"selected"`) {
				os.Exit(24)
			}
			request("fs/write_text_file", map[string]any{"sessionId": "fixture-session", "path": filepath.Join(dir, "wire.txt"), "content": "alpha\nbeta\ngamma"})
			content := request("fs/read_text_file", map[string]any{"sessionId": "fixture-session", "path": filepath.Join(dir, "wire.txt"), "line": 2, "limit": 1})
			if string(content["content"]) != `"beta"` {
				os.Exit(25)
			}
			created := request("terminal/create", map[string]any{"sessionId": "fixture-session", "command": "sh", "args": []string{"-c", "printf 'terminal result'; exit 4"}})
			var terminal string
			_ = json.Unmarshal(created["terminalId"], &terminal)
			terminalParams := map[string]string{"sessionId": "fixture-session", "terminalId": terminal}
			exit := request("terminal/wait_for_exit", terminalParams)
			if string(exit["exitCode"]) != "4" {
				os.Exit(26)
			}
			output := request("terminal/output", terminalParams)
			if string(output["output"]) != `"terminal result"` {
				os.Exit(27)
			}
			request("terminal/release", terminalParams)
			// Tool content is an array, unlike agent message content. It must not break decoding.
			_ = write.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "fixture-session", "update": map[string]any{"sessionUpdate": "tool_call", "title": "Fixture", "content": []any{map[string]string{"type": "terminal", "terminalId": terminal}}}}})
			for _, chunk := range []string{"Finished.\nRELEASE_", "RESULT {\"status\":\"released\",\"tag\":\"v1\",\"commit\":\"fixture\"}\n"} {
				_ = write.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "fixture-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": chunk}}}})
			}
			result = map[string]string{"stopReason": "end_turn"}
		case "$/cancel_request", "session/cancel":
			continue
		default:
			os.Exit(28)
		}
		if err := write.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}); err != nil {
			os.Exit(29)
		}
	}
}
func TestAgentProcessInteroperabilityAndTimeout(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Directory = t.TempDir()
	cfg.StateDirectory = t.TempDir()
	cfg.Agent = AgentConfig{Command: []string{executable, "-test.run=^TestWirePeer$"}, Environment: map[string]string{"RELEASE_BOT_WIRE_PEER": "normal"}, AuthMethod: "fixture", Mode: "fixture"}
	agent := agentProcess{config: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := agent.Execute(ctx, "fixture prompt")
	if err != nil {
		t.Fatal(err)
	}
	if r.Tag != "v1" || r.Status != "released" {
		t.Fatalf("lost streamed receipt: %+v", r)
	}
	data, err := os.ReadFile(filepath.Join(cfg.Directory, "wire.txt"))
	if err != nil || string(data) != "alpha\nbeta\ngamma" {
		t.Fatalf("file callback failed: %q %v", data, err)
	}
	agent.config.Agent.Environment["RELEASE_BOT_WIRE_PEER"] = "hang"
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	start := time.Now()
	if _, err := agent.Execute(ctx2, "hang"); err == nil {
		t.Fatal("timeout was ignored")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("agent process leaked after timeout")
	}
}
func TestWorkspaceCannotEscapeAndCancelledPermission(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "private")
	writeTestFile(t, path, "secret")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	h, err := newHost(context.Background(), dir, io.Discard, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	h.session = "s"
	for _, path := range []string{path, filepath.Join(dir, "link", "private"), "relative"} {
		params, _ := json.Marshal(map[string]string{"sessionId": "s", "path": path, "content": "overwrite"})
		for _, method := range []string{"fs/read_text_file", "fs/write_text_file"} {
			if _, err := h.request(context.Background(), method, params); err == nil {
				t.Fatalf("escaped root using %s %s", method, path)
			}
		}
	}
	h.cancel()
	params := json.RawMessage(`{"sessionId":"s","options":[{"kind":"allow_once","optionId":"yes"}]}`)
	result, err := h.request(context.Background(), "session/request_permission", params)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	if !strings.Contains(string(b), "cancelled") {
		t.Fatal("permission approved after cancellation")
	}
}
