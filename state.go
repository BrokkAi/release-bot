package releasebot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Result struct {
	Status string           `json:"status"`
	Tag    string           `json:"tag"`
	Commit string           `json:"commit"`
	URL    string           `json:"url,omitempty"`
	Detail string           `json:"detail,omitempty"`
	Plan   *PublicationPlan `json:"plan,omitempty"`
}
type Job struct {
	Target           string           `json:"target"`
	WorkBranch       string           `json:"work_branch,omitempty"`
	Started          time.Time        `json:"started"`
	Tries            int              `json:"tries"`
	RetryAt          time.Time        `json:"retry_at"`
	Failure          string           `json:"failure,omitempty"`
	SetupFailure     string           `json:"setup_failure,omitempty"`
	Interruption     string           `json:"interruption,omitempty"`
	Candidate        *Result          `json:"candidate,omitempty"`
	Phase            string           `json:"phase"`
	Plan             *PublicationPlan `json:"plan,omitempty"`
	BuildChecks      map[string]bool  `json:"build_checks,omitempty"`
	ValidatedAt      time.Time        `json:"validated_at,omitempty"`
	NeedsPreparation bool             `json:"needs_preparation,omitempty"`
}
type State struct {
	Format         int       `json:"format"`
	Remote         string    `json:"remote"`
	Branch         string    `json:"branch"`
	Directory      string    `json:"directory"`
	Released       string    `json:"released"`
	ReleasedAt     time.Time `json:"released_at"`
	Observed       string    `json:"observed"`
	ObservedRemote string    `json:"observed_remote,omitempty"`
	ChangedAt      time.Time `json:"changed_at"`
	Job            *Job      `json:"job,omitempty"`
	LastResult     *Result   `json:"last_result,omitempty"`
}

func ReadState(cfg Config) (*State, error) {
	data, err := os.ReadFile(filepath.Join(cfg.StateDirectory, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid state, refusing to guess publication status: %w", err)
	}
	if s.Format != 1 || s.Remote != cfg.Remote || s.Branch != cfg.Branch || s.Directory != cfg.Directory {
		return nil, errors.New("state version or repository identity does not match this configuration")
	}
	return &s, nil
}
func writeState(cfg Config, state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(cfg.StateDirectory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(cfg.StateDirectory, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(cfg.StateDirectory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another process holds %s: %w", path, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func lockConfig(cfg Config) (func(), error) {
	stateUnlock, err := lockFile(filepath.Join(cfg.StateDirectory, "daemon.lock"))
	if err != nil {
		return nil, err
	}
	checkoutUnlock, err := lockFile(cfg.Directory + ".release-bot.lock")
	if err != nil {
		stateUnlock()
		return nil, err
	}
	return func() { checkoutUnlock(); stateUnlock() }, nil
}
func Retry(cfg Config) error {
	unlock, err := lockConfig(cfg)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadState(cfg)
	if err != nil {
		return err
	}
	if s == nil || s.Job == nil {
		return errors.New("no pending release")
	}
	s.Job.Tries = 0
	s.Job.RetryAt = time.Time{}
	return writeState(cfg, s)
}
