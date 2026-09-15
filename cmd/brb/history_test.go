package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bot "github.com/BrokkAi/release-bot"
)

func TestHistoryOfflineAndSelection(t *testing.T) {
	root := t.TempDir()
	cfg := bot.DefaultConfig()
	cfg.Remote = "https://github.com/example/offline.git"
	cfg.Directory = filepath.Join(root, "checkout")
	cfg.StateDirectory = filepath.Join(root, "state")
	cfg.Agent.Command = []string{"must-not-run"}
	cfg.Verify = []string{"must-not-run"}
	cfg.Preflight = []string{"must-not-run"}
	if err := os.Mkdir(cfg.StateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return data
	}
	configFile := filepath.Join(root, "config.json")
	write(configFile, cfg)
	var err error
	cfg, err = bot.ReadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state := &bot.State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory}
	for _, tag := range []string{"v1", "v2"} {
		state.History = append(state.History, bot.VerifiedReceipt{VerifiedAt: &at, Result: bot.Result{Status: "released", Tag: tag, Commit: "commit-" + tag, URL: "https://example.invalid/" + tag, Plan: &bot.PublicationPlan{Tag: tag, Destinations: []bot.Destination{{Name: "package", Version: tag, Checks: []bot.PublishabilityCheck{{Evidence: "evidence-" + tag}}}}}}})
	}
	stateFile := filepath.Join(cfg.StateDirectory, "state.json")
	original := write(stateFile, state)
	t.Setenv("PATH", t.TempDir()) // No git, gh, agent or verifier available.
	for _, args := range [][]string{{"history", "--config", configFile}, {"history", "--config", configFile, "--json"}, {"history", "--config", configFile, "--tag", "v1"}} {
		err := executeWithRun(context.Background(), args, slog.New(slog.NewTextHandler(io.Discard, nil)), func(context.Context, bot.Config, *slog.Logger, bool, bool) error {
			t.Fatal("history launched run")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tag := range []string{"v1", "v2"} {
		var out bytes.Buffer
		if err := historyCommand(cfg, tag, false, &out); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Historical daemon verification succeeded", "agent-supplied", "commit-" + tag, "evidence-" + tag, "https://example.invalid/" + tag, `"version": "` + tag + `"`} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("missing %q: %s", want, out.String())
			}
		}
		out.Reset()
		if err := historyCommand(cfg, tag, true, &out); err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Receipts []bot.VerifiedReceipt `json:"receipts"`
		}
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || len(decoded.Receipts) != 1 || decoded.Receipts[0].Result.Tag != tag {
			t.Fatalf("invalid selection JSON: %s %v", out.String(), err)
		}
	}
	if err := historyCommand(cfg, "missing", true, io.Discard); err == nil {
		t.Fatal("missing tag accepted")
	}
	after, err := os.ReadFile(stateFile)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("history modified state")
	}
	entries, err := os.ReadDir(cfg.StateDirectory)
	if err != nil || len(entries) != 1 {
		t.Fatal("history wrote extra files")
	}
	cfg.Branch = "other"
	if err := historyCommand(cfg, "", true, io.Discard); err == nil {
		t.Fatal("history ignored repository identity")
	}
}

func TestHistoryEmptyAndLegacy(t *testing.T) {
	cfg := bot.DefaultConfig()
	cfg.StateDirectory = t.TempDir()
	var out bytes.Buffer
	if err := historyCommand(cfg, "", true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"receipts": []`) {
		t.Fatalf("empty history: %s", out.String())
	}
	state := bot.State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, LastResult: &bot.Result{Status: "released", Tag: "legacy", Commit: "old"}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDirectory, "state.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := historyCommand(cfg, "legacy", false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unknown verification time") {
		t.Fatal("missing legacy time invented")
	}
	out.Reset()
	if err := historyCommand(cfg, "legacy", true, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "verified_at") {
		t.Fatal("missing timestamp must be omitted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("legacy read modified state")
	}
}
