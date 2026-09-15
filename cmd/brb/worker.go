package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"

	bot "github.com/BrokkAi/release-bot"
	"github.com/BrokkAi/release-bot/internal/worker"
)

func workerCommand(ctx context.Context, args []string, version string) error {
	fs := flag.NewFlagSet("brb worker", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: brb worker --socket PATH\n\nServe versioned one-shot release checks to Brokk Town over a private Unix socket.")
		fs.PrintDefaults()
	}
	socket := fs.String("socket", "", "private Unix-domain socket path (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || *socket == "" {
		return fmt.Errorf("worker requires exactly one --socket PATH")
	}
	run := func(ctx context.Context, request worker.Request, progress func(worker.Progress)) (worker.Result, error) {
		ctx = bot.WithProgress(ctx, func(p bot.Progress) {
			progress(worker.Progress{Phase: p.Phase, Task: p.Task})
		})
		return worker.Result{}, bot.Run(ctx, workerConfig(request), slog.Default(), true, false)
	}
	retry := func(_ context.Context, request worker.Request) error {
		return bot.Retry(workerConfig(request))
	}
	return worker.Serve(ctx, *socket, worker.Initialize{
		Protocol: worker.ProtocolVersion, MinimumProtocol: worker.MinimumProtocol,
		Bot: "release-bot", Version: version, Capabilities: []string{"run", "progress", "release", "retry"},
	}, run, retry, slog.Default())
}

// workerConfig applies this bot's defaults to the workspace Town named.
func workerConfig(request worker.Request) bot.Config {
	cfg := bot.DefaultConfig()
	cfg.Remote = request.Remote
	cfg.Branch = request.Branch
	cfg.Directory = request.Directory
	cfg.StateDirectory = request.StateDirectory
	cfg.Agent = request.Agent
	cfg.GitHub.Repo = request.Repo
	cfg.GitHub.Host = request.Host
	cfg.Verify = request.Verify
	return cfg
}
