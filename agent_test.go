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
	modelSelected := false
	modelOptionID := "provider-model"
	modelCategory := "model"
	if mode == "model-no-category" {
		modelOptionID, modelCategory = "model", ""
	}
	modelOptions := func(current string) []any {
		return []any{map[string]any{"id": modelOptionID, "name": "Model", "type": "select", "category": modelCategory, "currentValue": current, "options": []any{map[string]string{"value": "fixture-small", "name": "Small"}, map[string]string{"value": "fixture-large", "name": "Large"}}}}
	}
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
			result = map[string]any{"sessionId": "fixture-session"}
			if strings.HasPrefix(mode, "model") && mode != "model-unsupported" {
				result = map[string]any{"sessionId": "fixture-session", "configOptions": modelOptions("fixture-small")}
			}
		case "session/set_config_option":
			var p struct{ SessionID, ConfigID, Value string }
			if json.Unmarshal(m.Params, &p) != nil || p.SessionID != "fixture-session" || p.ConfigID != modelOptionID || p.Value != "fixture-large" {
				os.Exit(30)
			}
			if mode == "model-rejected" {
				_ = write.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32602, "message": "model access denied"}})
				continue
			}
			modelSelected = true
			current := p.Value
			if mode == "model-unconfirmed" {
				current = "fixture-small"
			}
			result = map[string]any{"configOptions": modelOptions(current)}
		case "session/prompt":
			if strings.HasPrefix(mode, "model") && !modelSelected {
				os.Exit(31)
			}
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

func TestAgentSelectsModelBeforeReleaseWork(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, model, failure string }{
		{"model", "fixture-large", ""},
		{"model-no-category", "fixture-large", ""},
		{"model", "unknown-model", "available values: fixture-small, fixture-large"},
		{"model-unsupported", "fixture-large", "does not advertise ACP model selection"},
		{"model-rejected", "fixture-large", "model access denied"},
		{"model-unconfirmed", "fixture-large", "did not confirm"},
	} {
		t.Run(tc.mode+"/"+tc.model, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Directory, cfg.StateDirectory = t.TempDir(), t.TempDir()
			cfg.Agent = AgentConfig{Command: []string{executable, "-test.run=^TestWirePeer$"}, Environment: map[string]string{"RELEASE_BOT_WIRE_PEER": tc.mode}, Model: tc.model}
			a := agentProcess{config: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := a.Execute(ctx, "fixture release work")
			if tc.failure == "" {
				if err != nil || result.Status != "released" {
					t.Fatalf("model selection failed: %+v %v", result, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("expected %q, got %v", tc.failure, err)
				}
				if _, err := os.Stat(filepath.Join(cfg.Directory, "wire.txt")); !os.IsNotExist(err) {
					t.Fatal("agent received work after failed model selection")
				}
			}
		})
	}
}
func TestAgentPersistsCancellationSource(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Directory, cfg.StateDirectory = t.TempDir(), t.TempDir()
	cfg.Agent = AgentConfig{Command: []string{executable, "-test.run=^TestWirePeer$"}, Environment: map[string]string{"RELEASE_BOT_WIRE_PEER": "hang"}}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	// Cancel only once the prompt is recorded, not after an arbitrary delay.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	finished := make(chan error, 1)
	go func() { _, err := (agentProcess{config: cfg, log: log}).Execute(ctx, "fixture"); finished <- err }()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var path string
	waiting := true
	for waiting {
		select {
		case <-deadline:
			cancel(errors.New("test timed out waiting for prompt"))
			<-finished
			t.Fatal("agent never reached the prompt")
		case <-tick.C:
			files, _ := filepath.Glob(filepath.Join(cfg.StateDirectory, "sessions", "*.jsonl"))
			if len(files) == 1 {
				path = files[0]
				data, _ := os.ReadFile(path)
				waiting = !strings.Contains(string(data), `"prompt":"fixture"`)
			}
		}
	}
	cancel(errors.New("fixture received SIGTERM"))
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == "session_end" {
			found = true
			if record["phase"] != "session/prompt" || record["context_cause"] != "fixture received SIGTERM" || record["error"] != "context canceled" {
				t.Fatalf("lost cancellation source: %+v", record)
			}
		}
	}
	if !found {
		t.Fatal("session has no termination record")
	}
}
