package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	bot "github.com/BrokkAi/release-bot"
)

func cliRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git(root, "init", "--bare", remote)
	git(remote, "symbolic-ref", "HEAD", "refs/heads/main")
	git(root, "init", "-b", "main", source)
	git(source, "commit", "--allow-empty", "-m", "initial")
	git(source, "remote", "add", "origin", remote)
	git(source, "push", "origin", "main")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex-acp"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	return source
}
func TestNoArgumentsStartsWorkWithoutConfiguration(t *testing.T) {
	source := cliRepository(t)
	t.Chdir(source)
	called := false
	err := executeWithRun(context.Background(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ctx context.Context, cfg bot.Config, log *slog.Logger, once, force bool) error {
		called = true
		if cfg.Branch != "main" || once || force {
			t.Fatalf("unexpected default startup: %+v once=%v force=%v", cfg, once, force)
		}
		if _, err := os.Stat("release-bot.json"); !os.IsNotExist(err) {
			t.Fatal("startup required or created a config file")
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("did not start work: %v", err)
	}
}
func TestRepositoryAndFlagsInEitherOrder(t *testing.T) {
	source := cliRepository(t)
	for _, args := range [][]string{{source, "--once", "--force"}, {"--once", source, "--force"}, {"once", "--force", source}, {"run", source, "--once", "--force"}} {
		called := false
		err := executeWithRun(context.Background(), args, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ctx context.Context, cfg bot.Config, log *slog.Logger, once, force bool) error {
			called = true
			if !once || !force {
				t.Fatal("flags lost")
			}
			return nil
		})
		if err != nil || !called {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
func TestHelpDoesNotNeedRepositoryAndConsoleIsReadable(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := execute(context.Background(), []string{"--help"}, slog.Default()); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	slog.New(newConsole(&output)).Info("Watching repository", "branch", "main")
	if !strings.Contains(output.String(), "Watching repository · branch: main") || strings.Contains(output.String(), "msg=") {
		t.Fatalf("unexpected console output: %s", output.String())
	}
}
