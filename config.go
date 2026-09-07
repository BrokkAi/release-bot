package releasebot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BrokkAi/acp-go/runner"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(value)
	if err == nil {
		*d = Duration(parsed)
	}
	return err
}
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

type AgentConfig = runner.AgentConfig
type GitHubConfig struct {
	Repo      string   `json:"repo,omitempty"`
	Host      string   `json:"host"`
	Workflows []string `json:"workflows"`
	Assets    []string `json:"assets"`
}
type Config struct {
	Remote              string       `json:"remote"`
	Branch              string       `json:"branch"`
	Directory           string       `json:"directory"`
	StateDirectory      string       `json:"state_directory"`
	InitialRef          string       `json:"initial_ref,omitempty"`
	InstructionFiles    []string     `json:"instruction_files"`
	Agent               AgentConfig  `json:"agent"`
	GitHub              GitHubConfig `json:"github"`
	Poll                Duration     `json:"poll"`
	Daily               Duration     `json:"daily"`
	MinimumGap          Duration     `json:"minimum_gap"`
	Quiet               Duration     `json:"quiet"`
	BurstWindow         Duration     `json:"burst_window"`
	Burst               int          `json:"burst"`
	Timeout             Duration     `json:"timeout"`
	VerificationTimeout Duration     `json:"verification_timeout"`
	RetryDelay          Duration     `json:"retry_delay"`
	Attempts            int          `json:"attempts"`
	Verify              []string     `json:"verify,omitempty"`
	Preflight           []string     `json:"preflight,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Branch: "master", Directory: "var/checkout", StateDirectory: "var/state",
		InstructionFiles: []string{"AGENTS.md", "RELEASING.md", "RELEASE.md", "CONTRIBUTING.md"},
		Agent:            AgentConfig{Command: []string{"codex-acp"}}, GitHub: GitHubConfig{Host: "github.com"},
		Poll: Duration(5 * time.Minute), Daily: Duration(24 * time.Hour), MinimumGap: Duration(2 * time.Hour), Quiet: Duration(15 * time.Minute), BurstWindow: Duration(2 * time.Hour), Burst: 5,
		Timeout: Duration(2 * time.Hour), VerificationTimeout: Duration(30 * time.Minute), RetryDelay: Duration(15 * time.Minute), Attempts: 3,
	}
}
func ReadConfig(filename string) (Config, error) {
	cfg := DefaultConfig()
	f, err := os.Open(filename)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("expected one configuration object")
	}
	base, err := filepath.Abs(filepath.Dir(filename))
	if err != nil {
		return cfg, err
	}
	for _, path := range []*string{&cfg.Directory, &cfg.StateDirectory} {
		if !filepath.IsAbs(*path) {
			*path = filepath.Join(base, *path)
		}
		*path, err = canonical(*path)
		if err != nil {
			return cfg, err
		}
	}
	return cfg, cfg.Validate()
}

// Resolve existing ancestors, including symlinks, even before clone creation.
func canonical(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	parent, err = canonical(parent)
	return filepath.Join(parent, filepath.Base(path)), err
}

var slug = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (c Config) GitHubRepo() string {
	if c.GitHub.Repo != "" {
		return c.GitHub.Repo
	}
	path := ""
	if prefix := "git@" + c.GitHub.Host + ":"; strings.HasPrefix(c.Remote, prefix) {
		path = strings.TrimPrefix(c.Remote, prefix)
	}
	if u, err := url.Parse(c.Remote); err == nil && u.Hostname() == c.GitHub.Host {
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if slug.MatchString(path) {
		return path
	}
	return ""
}
func (c Config) Validate() error {
	if c.Remote == "" || strings.HasPrefix(c.Remote, "-") {
		return errors.New("remote is required")
	}
	if c.Branch == "" || strings.HasPrefix(c.Branch, "-") || strings.Contains(c.Branch, "..") {
		return errors.New("invalid branch")
	}
	if c.Directory == "" || c.StateDirectory == "" {
		return errors.New("directory and state_directory are required")
	}
	for _, pair := range [][2]string{{c.Directory, c.StateDirectory}, {c.StateDirectory, c.Directory}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return err
		}
		if rel == "." || filepath.IsLocal(rel) {
			return errors.New("checkout and state directories must not overlap")
		}
	}
	if len(c.Agent.Command) == 0 || c.Agent.Command[0] == "" {
		return errors.New("agent.command is required")
	}
	if c.Agent.Effort != "" && strings.TrimSpace(c.Agent.Effort) == "" {
		return errors.New("agent.effort requires a reasoning effort value")
	}
	if c.GitHub.Repo != "" && !slug.MatchString(c.GitHub.Repo) {
		return errors.New("github.repo must be owner/repository")
	}
	if c.GitHub.Host == "" || strings.ContainsAny(c.GitHub.Host, " /:\\") {
		return errors.New("invalid github.host")
	}
	if len(c.Verify) > 0 && c.Verify[0] == "" {
		return errors.New("verify command is empty")
	}
	if len(c.Preflight) > 0 && c.Preflight[0] == "" {
		return errors.New("preflight command is empty")
	}
	for _, s := range c.InstructionFiles {
		if !filepath.IsLocal(s) {
			return fmt.Errorf("instruction file must be relative: %s", s)
		}
	}
	for _, pattern := range c.GitHub.Assets {
		if _, err := filepath.Match(pattern, ""); err != nil {
			return err
		}
	}
	for _, name := range c.GitHub.Workflows {
		if name == "" {
			return errors.New("required workflow cannot be empty")
		}
	}
	for _, d := range []Duration{c.Poll, c.Daily, c.BurstWindow, c.Timeout, c.VerificationTimeout, c.RetryDelay} {
		if d <= 0 {
			return errors.New("poll, daily, burst_window, timeout, verification_timeout and retry_delay must be positive")
		}
	}
	if c.Quiet < 0 || c.MinimumGap < 0 || c.Burst < 0 || c.Attempts < 1 || c.MinimumGap > c.Daily {
		return errors.New("invalid schedule or retry limits")
	}
	return nil
}
