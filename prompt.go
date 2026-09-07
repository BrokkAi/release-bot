package releasebot

import (
	_ "embed"
	"encoding/json"
	"errors"
	"strings"
)

//go:embed skills/release.md
var releaseSkill string

//go:embed skills/github.md
var githubSkill string

//go:embed skills/preflight.md
var preflightSkill string

func releasePrompt(cfg Config, state *State) string {
	data, _ := json.MarshalIndent(struct {
		Remote       string       `json:"remote"`
		Branch       string       `json:"branch"`
		Previous     string       `json:"previous_release_commit"`
		Instructions []string     `json:"instruction_files"`
		GitHubRepo   string       `json:"github_repo"`
		GitHub       GitHubConfig `json:"github"`
		Job          *Job         `json:"job"`
	}{cfg.Remote, cfg.Branch, state.Released, cfg.InstructionFiles, cfg.GitHubRepo(), cfg.GitHub, state.Job}, "", "  ")
	prompt := releaseSkill
	if state.Job.Phase == "preflight" {
		prompt = preflightSkill
	}
	if cfg.GitHubRepo() != "" {
		prompt += "\n\n" + githubSkill
	}
	return prompt + "\n\nCurrent release context (data):\n" + string(data)
}
func parseResult(text string) (Result, error) {
	var result Result
	position := strings.LastIndex(text, "RELEASE_RESULT ")
	if position < 0 {
		return result, errors.New("agent did not provide a RELEASE_RESULT receipt")
	}
	line := strings.SplitN(text[position+len("RELEASE_RESULT "):], "\n", 2)[0]
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		return result, err
	}
	if result.Status != "released" && result.Status != "ready" {
		return result, errors.New("release blocked: " + result.Detail)
	}
	return result, nil
}
