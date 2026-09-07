package releasebot

import (
	"context"
	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/runner"
	"log/slog"
)

type Agent interface {
	Execute(context.Context, string) (Result, error)
}
type agentProcess struct {
	config Config
	log    *slog.Logger
}
type agentSetupError = runner.SetupError

func (a agentProcess) Execute(ctx context.Context, prompt string) (Result, error) {
	text, err := (runner.Runner{Config: runner.Config{
		Directory: a.config.Directory, StateDirectory: a.config.StateDirectory,
		Agent: a.config.Agent, AutoApprove: true,
		ClientInfo: acp.ClientInfo{Name: "release-bot", Version: "0.1.0"},
	}, Log: a.log}).Execute(ctx, prompt)
	if err != nil {
		return Result{}, err
	}
	return parseResult(text)
}
