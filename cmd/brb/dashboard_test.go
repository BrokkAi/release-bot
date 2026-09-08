package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	bot "github.com/BrokkAi/release-bot"
	"github.com/rivo/uniseg"
)

func dashboardFixture() *dashboard {
	cfg := bot.DefaultConfig()
	cfg.GitHub.Repo = "BrokkAi/release-bot"
	cfg.Branch = "main"
	cfg.Agent.Model = "review-model"
	cfg.StateDirectory = "/tmp/release-bot-state"
	d := newDashboard(cfg)
	d.update(bot.Progress{Phase: "starting"})
	d.update(bot.Progress{Phase: "validating", Task: "npm: authorization", Commit: strings.Repeat("a", 40), Attempt: 1, MaxAttempts: 3,
		LastTag: "v1.0.0", Unreleased: 3, Recent: 1, ChangesKnown: true, Releases: []bot.ReleaseProgress{
			{ID: "a", Title: "v1.0.0", Status: "released", URL: "https://github.com/BrokkAi/release-bot/releases/tag/v1.0.0", Body: strings.Repeat("Destination: npm package; verified publication.\n", 20)},
			{ID: "b", Title: "v1.1.0", Status: "validating"},
			{ID: "c", Title: "Wide text 日本語 👩‍💻 é breaks table sizing", Status: "pending"},
		}})
	d.completed, d.sessions, d.tools = 2, 4, 23
	d.tool = "go test ./..."
	for _, text := range []string{"Preparing release", "Checking npm package", "Verifying publisher authorization"} {
		d.addActivity("12:34:56 " + text)
	}
	return d
}

func TestDashboardFitsPaneAndUnicode(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {12, 6}, {40, 12}, {60, 18}, {80, 24}, {120, 40}} {
		for view := 0; view < 4; view++ {
			d := dashboardFixture()
			d.view = view % 3
			d.detail = view == 3
			d.selected = 2
			out := d.render(size[0], size[1], time.Now(), false)
			rows := strings.Split(out, "\r\n")
			if len(rows) != size[1] {
				t.Fatalf("%v view %d: %d rows", size, view, len(rows))
			}
			for _, row := range rows {
				if !utf8.ValidString(row) || uniseg.StringWidth(row) > max(1, size[0]-1) || strings.ContainsAny(row, "\x1b\n\r") {
					t.Fatalf("%v view %d: invalid row %q", size, view, row)
				}
			}
		}
	}
	d := dashboardFixture()
	text := d.render(80, 24, time.Now(), false)
	for _, want := range []string{"BrokkAi/release-bot", "main", "VALIDATING", "attempt 1/3", "v1.0.0", "3 unreleased commits", "1 recent", "2 completed", "4 agents", "23 tools", "go test ./..."} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in dashboard:\n%s", want, text)
		}
	}
}

func TestDashboardReleaseDetailsRemainSelected(t *testing.T) {
	d := dashboardFixture()
	d.key("2")
	d.key("j")
	d.key("j")
	d.key("enter")
	text := d.render(40, 18, time.Now(), false)
	if !strings.Contains(text, "VERIFIED") || !strings.Contains(text, "https://github.com/") {
		t.Fatalf("missing release detail:\n%s", text)
	}
	p := d.progress
	p.Releases = append(p.Releases, bot.ReleaseProgress{ID: "new", Title: "New result", Status: "pending"})
	d.update(p)
	if d.selected != 3 || !d.detail {
		t.Fatal("incoming result changed the open release")
	}
	d.key("G")
	_ = d.render(40, 18, time.Now(), false)
	if d.scroll == 0 {
		t.Fatal("detail did not scroll")
	}
	d.key("esc")
	if d.detail || d.scroll != 0 {
		t.Fatal("escape did not return to release list")
	}
}

func TestDashboardLogStreamsAndConcurrentUpdates(t *testing.T) {
	d := dashboardFixture()
	log := slog.New(&dashboardHandler{d: d})
	stream := log.With("source", "Agent", "stream_id", "session")
	stream.Info("agent transcript", "text", "Inspecting ")
	stream.Info("agent transcript", "text", "parser\nDone")
	if !strings.HasSuffix(d.activity[len(d.activity)-2], "Inspecting parser") {
		t.Fatal("transcript fragments were not joined")
	}
	log.Info("Tool", "title", "go test")
	log.Info("Tool completed", "title", "go test")
	if d.tool != "" {
		t.Fatal("completed tool remained active")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Go(func() {
			for j := 0; j < 100; j++ {
				log.Info("Tool", "title", "fixture\x1b[2J\x1b]52;c;clipboard\a")
			}
		})
	}
	for i := 0; i < 10; i++ {
		_ = d.render(40, 12, time.Now(), false)
	}
	wg.Wait()
	if len(d.activity) != activityLimit {
		t.Fatalf("unbounded activity: %d", len(d.activity))
	}
	for _, line := range d.activity {
		if strings.ContainsAny(line, "\x1b\a") || strings.Contains(line, "clipboard") {
			t.Fatal("terminal control sequence reached display")
		}
	}
}

func TestDashboardOutputModes(t *testing.T) {
	for _, tc := range []struct {
		plain, json, in, out bool
		term                 string
		want                 bool
	}{
		{false, false, true, true, "tmux-256color", true},
		{true, false, true, true, "xterm", false},
		{false, true, true, true, "xterm", false},
		{false, false, false, true, "xterm", false},
		{false, false, true, false, "xterm", false},
		{false, false, true, true, "dumb", false},
	} {
		if got := dashboardEnabled(tc.plain, tc.json, tc.in, tc.out, tc.term); got != tc.want {
			t.Errorf("mode %+v = %v", tc, got)
		}
	}
	if err := executeWithRun(context.Background(), []string{"--plain", "--json"}, slog.New(slog.DiscardHandler), nil); err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("conflicting output flags: %v", err)
	}
}

func TestKeyDecoderSplitSequencesAndPaste(t *testing.T) {
	var d keyDecoder
	if keys := d.feed("\x1b["); len(keys) != 0 {
		t.Fatalf("partial arrow became keys: %v", keys)
	}
	if keys := d.feed("A2\t\r"); strings.Join(keys, ",") != "up,2,tab,enter" {
		t.Fatalf("keys %v", keys)
	}
	if keys := d.feed("\x1b[200~q\x03jjj\x1b[20"); len(keys) != 0 {
		t.Fatalf("paste became shortcuts: %v", keys)
	}
	if keys := d.feed("1~q"); strings.Join(keys, ",") != "q" {
		t.Fatalf("paste terminator %v", keys)
	}
}

func TestDashboardNavigationBeforeFirstRelease(t *testing.T) {
	d := newDashboard(bot.DefaultConfig())
	d.key("j")
	d.update(bot.Progress{Releases: []bot.ReleaseProgress{{ID: "first", Title: "First result", Status: "pending"}}})
	d.key("enter")
	if text := d.render(40, 12, time.Now(), false); !strings.Contains(text, "First result") {
		t.Fatalf("first result was not selectable: %s", text)
	}
}

func TestDashboardSummaryRetainsLinksAndFailure(t *testing.T) {
	d := dashboardFixture()
	summary := d.summary(nil)
	if !strings.Contains(summary, "https://github.com/") || !strings.Contains(summary, "State: ") {
		t.Fatalf("summary lost result links: %s", summary)
	}
	p := d.progress
	p.Phase, p.Failure = "paused", "Verification failed"
	d.update(p)
	summary = d.summary(nil)
	if !strings.Contains(summary, " · paused · ") || !strings.Contains(summary, "Verification failed") {
		t.Fatalf("summary hid pending failure: %s", summary)
	}
}
