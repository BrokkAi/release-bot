package releasebot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A real stdio peer verifies selection order and prevents prompts before an
// effort acknowledgement. No model or external service is involved.
func TestEffortWirePeer(t *testing.T) {
	mode := os.Getenv("RELEASE_BOT_EFFORT_PEER")
	if mode == "" {
		return
	}
	input, output := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	id, category := "provider-thinking", "thought_level"
	if mode == "no-category" {
		id, category = "reasoning_effort", ""
	}
	selected := false
	model := "small"
	effort := "low"
	options := func() []any {
		modelOption := map[string]any{"id": "model", "name": "Model", "type": "select", "category": "model", "currentValue": model, "options": []any{map[string]string{"value": "small"}, map[string]string{"value": "large"}}}
		if mode == "unsupported" {
			return []any{modelOption}
		}
		choices := []any{map[string]string{"value": "low"}}
		if mode != "with-model" || model == "large" {
			choices = append(choices, map[string]string{"value": "high"})
		}
		return []any{modelOption, map[string]any{"id": id, "name": "Reasoning effort", "type": "select", "category": category, "currentValue": effort, "options": choices}}
	}
	dir := ""
	for {
		var m struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		if input.Decode(&m) != nil {
			os.Exit(0)
		}
		var result any = map[string]any{}
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1}
		case "session/new":
			var p struct{ Cwd string }
			_ = json.Unmarshal(m.Params, &p)
			dir = p.Cwd
			result = map[string]any{"sessionId": "effort-session", "configOptions": options()}
		case "session/set_config_option":
			var p struct{ SessionID, ConfigID, Value string }
			if json.Unmarshal(m.Params, &p) != nil || p.SessionID != "effort-session" || mode == "default" {
				os.Exit(41)
			}
			if p.ConfigID == "model" {
				if mode != "with-model" || p.Value != "large" || selected {
					os.Exit(42)
				}
				model = p.Value
			} else {
				if p.ConfigID != id || p.Value != "high" || (mode == "with-model" && model != "large") {
					os.Exit(43)
				}
				if mode == "rejected" {
					_ = output.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32602, "message": "effort access denied"}})
					continue
				}
				selected = true
				if mode != "unconfirmed" {
					effort = p.Value
				}
			}
			result = map[string]any{"configOptions": options()}
		case "session/prompt":
			_ = os.WriteFile(filepath.Join(dir, "prompted"), []byte("yes"), 0600)
			if mode != "default" && (!selected || effort != "high") {
				os.Exit(44)
			}
			_ = output.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "effort-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "RELEASE_RESULT {\"status\":\"ready\",\"detail\":\"effort confirmed\"}\n"}}}})
			result = map[string]string{"stopReason": "end_turn"}
		default:
			os.Exit(45)
		}
		if output.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}) != nil {
			os.Exit(46)
		}
	}
}

func TestAgentSelectsEffortAfterModelBeforeWork(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, model, effort, failure string }{
		{"normal", "", "high", ""},
		{"with-model", "large", "high", ""},
		{"no-category", "", "high", ""},
		{"default", "", "", ""},
		{"normal", "", "unknown", "available values: low, high"},
		{"unsupported", "", "high", "does not advertise ACP reasoning effort selection"},
		{"rejected", "", "high", "effort access denied"},
		{"unconfirmed", "", "high", "did not confirm"},
	} {
		t.Run(tc.mode+"/"+tc.effort, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Directory, cfg.StateDirectory = t.TempDir(), t.TempDir()
			cfg.Agent = AgentConfig{Command: []string{executable, "-test.run=^TestEffortWirePeer$"}, Environment: map[string]string{"RELEASE_BOT_EFFORT_PEER": tc.mode}, Model: tc.model, Effort: tc.effort}
			agent := agentProcess{config: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := agent.Execute(ctx, "fixture work")
			if tc.failure == "" {
				if err != nil || result.Detail != "effort confirmed" {
					t.Fatalf("selection failed: %+v %v", result, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("want %q got %v", tc.failure, err)
				}
				if _, err := os.Stat(filepath.Join(cfg.Directory, "prompted")); !os.IsNotExist(err) {
					t.Fatal("work started despite rejected effort")
				}
			}
		})
	}
}
