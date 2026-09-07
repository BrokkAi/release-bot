package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	bot "github.com/BrokkAi/release-bot"
)

func main() {
	logger := slog.New(newConsole(os.Stderr))
	for _, arg := range os.Args[1:] {
		if arg == "--json" || arg == "-json" || arg == "--json=true" || arg == "-json=true" {
			logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := execute(ctx, os.Args[1:], logger)
	if ctx.Err() != nil {
		logger.Info("Stopped", "reason", context.Cause(ctx))
		return
	}
	if err != nil {
		logger.Error("stopped", "error", err)
		os.Exit(1)
	}
}
func execute(ctx context.Context, args []string, logger *slog.Logger) error {
	return executeWithRun(ctx, args, logger, bot.Run)
}

type runFunc func(context.Context, bot.Config, *slog.Logger, bool, bool) error

func executeWithRun(ctx context.Context, args []string, logger *slog.Logger, run runFunc) error {
	mode := "run"
	if len(args) > 0 {
		switch args[0] {
		case "run", "once", "status", "retry":
			mode = args[0]
			args = args[1:]
		}
	}
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: release-bot [repository path or URL] [options]\n\nRun inside a repository to detect its remote and default branch and start working.\nNo configuration file is needed. Repository instructions and checks are discovered\nby the agent. Existing run, once, status and retry commands are also supported.\n\nOptions:")
		flags.PrintDefaults()
	}
	file := flags.String("config", "", "optional JSON configuration for advanced settings")
	branch := flags.String("branch", "", "branch to release (default: repository's default branch)")
	selectedAgent := flags.String("agent", "", "ACP agent executable (default: Codex)")
	model := flags.String("model", "", "model ID to use (default: agent's configured model)")
	effort := flags.String("effort", "", "reasoning effort to use, such as low, medium or high (default: agent's configured effort)")
	burst := flags.Int("burst", bot.DefaultConfig().Burst, "unreleased commits within the burst window to trigger an early release; 0 disables (overrides config; minimum gap and quiet period still apply)")
	var agentArgs []string
	flags.Func("agent-arg", "argument for the agent; repeat as needed", func(value string) error { agentArgs = append(agentArgs, value); return nil })
	once := flags.Bool("once", mode == "once", "check/work once, then exit")
	jsonOutput := flags.Bool("json", false, "emit JSON logs and status")
	force := flags.Bool("force", false, "ignore cadence in once mode; still requires unreleased commits")
	if err := parseInterspersed(flags, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("pass one repository path or URL")
	}
	if *force && !*once {
		return errors.New("use --once with --force")
	}
	if (mode == "status" || mode == "retry") && (*force || *once) {
		return errors.New("--once and --force apply to run or once")
	}
	var cfg bot.Config
	var err error
	if *file != "" {
		if flags.NArg() != 0 {
			return errors.New("use a repository argument or --config, not both")
		}
		cfg, err = bot.ReadConfig(*file)
		if *branch != "" {
			cfg.Branch = *branch
		}
	} else {
		cfg, err = bot.Discover(ctx, flags.Arg(0), *branch)
	}
	if err != nil {
		return err
	}
	if *selectedAgent != "" {
		cfg.Agent.Command = []string{*selectedAgent}
	}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "burst" {
			cfg.Burst = *burst
			if *burst < 0 {
				err = errors.New("--burst must be zero or greater")
			}
		}
		if f.Name == "model" {
			cfg.Agent.Model = *model
			if strings.TrimSpace(*model) == "" {
				err = errors.New("--model requires a model ID")
			}
		}
		if f.Name == "effort" {
			cfg.Agent.Effort = *effort
			if strings.TrimSpace(*effort) == "" {
				err = errors.New("--effort requires a reasoning effort value")
			}
		}
	})
	if err != nil {
		return err
	}
	cfg.Agent.Command = append(cfg.Agent.Command, agentArgs...)
	if err := cfg.Validate(); err != nil {
		return err
	}
	switch mode {
	case "status":
		state, err := bot.ReadState(cfg)
		if err != nil {
			return err
		}
		if *jsonOutput {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(state)
		}
		fmt.Fprintf(os.Stdout, "Repository: %s\nBranch: %s\n", cfg.Remote, cfg.Branch)
		if state == nil {
			fmt.Fprintln(os.Stdout, "No releases recorded yet.")
			return nil
		}
		if state.LastResult != nil {
			fmt.Fprintf(os.Stdout, "Last release: %s\n", state.LastResult.Tag)
		}
		if state.Job != nil {
			fmt.Fprintf(os.Stdout, "In progress: %s (attempt %d/%d)\n", state.Job.Phase, state.Job.Tries, cfg.Attempts)
			if state.Job.Failure != "" {
				fmt.Fprintln(os.Stdout, state.Job.Failure)
			}
			if state.Job.SetupFailure != "" {
				fmt.Fprintln(os.Stdout, "Agent setup:", state.Job.SetupFailure)
			}
			if state.Job.Interruption != "" {
				fmt.Fprintln(os.Stdout, "Interrupted:", state.Job.Interruption)
			}
		} else {
			fmt.Fprintln(os.Stdout, "No pending release.")
		}
		return nil
	case "retry":
		if err := bot.Retry(cfg); err != nil {
			return err
		}
		logger.Info("Resuming the pending release")
	}
	if err := bot.ResolveAgent(&cfg, *file == "" && *selectedAgent == ""); err != nil {
		return err
	}
	if cfg.GitHubRepo() != "" {
		if _, err := exec.LookPath("gh"); err != nil {
			return errors.New("GitHub CLI is required for this repository; install gh and run gh auth login")
		}
	}
	logger.Info("Watching repository", "repository", cfg.Remote, "branch", cfg.Branch)
	logger.Info("Using agent", "command", strings.Join(cfg.Agent.Command, " "))
	if cfg.Agent.Model != "" || cfg.Agent.Effort != "" {
		logger.Info("Requested agent settings", "model", cfg.Agent.Model, "effort", cfg.Agent.Effort)
	}
	logger.Info("Using managed workspace", "checkout", cfg.Directory, "state", cfg.StateDirectory)
	return run(ctx, cfg, logger, *once, *force)
}

// Accept the repository before or after flags, as users expect from CLI tools.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		options = append(options, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	return fs.Parse(append(append(options, "--"), positional...))
}
