package releasebot

import (
	"context"
	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/runner"
	"log/slog"
)

// Agent runs one prompt per session. Execute performs release work and returns
// its receipt; Triage answers whether unreleased commits warrant an early release.
type Agent interface {
	Execute(context.Context, string) (Result, error)
	Triage(context.Context, string) (TriageDecision, error)
}
type agentProcess struct {
	config Config
	log    *slog.Logger
}
type agentSetupError = runner.SetupError

func (a agentProcess) run(ctx context.Context, prompt string) (string, error) {
	return (runner.Runner{Config: runner.Config{
		Directory: a.config.Directory, StateDirectory: a.config.StateDirectory,
		Agent: a.config.Agent, AutoApprove: true,
		ClientInfo: acp.ClientInfo{Name: "release-bot", Version: "0.1.0"},
	}, Log: a.log}).Execute(ctx, prompt)
}
func (a agentProcess) Execute(ctx context.Context, prompt string) (Result, error) {
	text, err := a.run(ctx, prompt)
	if err != nil {
		return Result{}, err
	}
	return parseResult(text)
}
func (a agentProcess) Triage(ctx context.Context, prompt string) (TriageDecision, error) {
	text, err := a.run(ctx, prompt)
	if err != nil {
		return TriageDecision{}, err
	}
	return parseTriage(text)
}
