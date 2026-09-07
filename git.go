package releasebot

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BrokkAi/release-bot/internal/osrun"
)

type checkout struct{ config Config }

func (g checkout) git(ctx context.Context, args ...string) (string, error) {
	return osrun.Run(ctx, g.config.Directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"}, append([]string{"git"}, args...)...)
}
func (g checkout) branchRef() string { return "refs/remotes/origin/" + g.config.Branch }
func (g checkout) repositoryDirectory() string {
	return filepath.Join(g.config.StateDirectory, "repository.git")
}

// New worktrees belong to a private repository, never the checkout from which
// brb was invoked. Branches, fetches, tags and Git configuration stay isolated.
func (g checkout) create(ctx context.Context) error {
	repository := g.repositoryDirectory()
	if err := os.MkdirAll(g.config.StateDirectory, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.config.Directory), 0700); err != nil {
		return err
	}
	if _, err := os.Stat(repository); errors.Is(err, os.ErrNotExist) {
		if _, err := osrun.Run(ctx, "", map[string]string{"GIT_TERMINAL_PROMPT": "0"}, "git", "clone", "--bare", "--no-hardlinks", "--branch", g.config.Branch, "--origin", "origin", "--", g.config.Remote, repository); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	git := func(args ...string) (string, error) {
		return osrun.Run(ctx, "", map[string]string{"GIT_TERMINAL_PROMPT": "0"}, append([]string{"git", "--git-dir", repository}, args...)...)
	}
	bare, err := git("rev-parse", "--is-bare-repository")
	if err != nil {
		return err
	}
	remote, err := git("remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if bare != "true" || remote != g.config.Remote {
		return errors.New("private worktree repository does not match the configured remote")
	}
	// Bare clones have no default fetch mapping. Keep ordinary agent fetches
	// useful for PR branches too, without updating any local branch.
	if _, err := git("config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return err
	}
	if _, err := git("fetch", "--tags", "origin", "+refs/heads/"+g.config.Branch+":"+g.branchRef()); err != nil {
		return err
	}
	_, err = git("worktree", "add", "--detach", g.config.Directory, g.branchRef())
	return err
}

func (g checkout) open(ctx context.Context) error {
	if _, err := os.Stat(g.config.Directory); errors.Is(err, os.ErrNotExist) {
		if err := g.create(ctx); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	root, err := g.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if root != g.config.Directory {
		return fmt.Errorf("directory must be the root of its own checkout: %s", root)
	}
	common, err := g.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common, err = canonical(common)
	if err != nil {
		return err
	}
	// Keep existing standalone clones, including pending jobs, in place. Reject
	// linked worktrees that would let the agent mutate another bot's Git state.
	if common != filepath.Join(g.config.Directory, ".git") && common != g.repositoryDirectory() {
		return errors.New("checkout shares Git metadata outside this bot's workspace; use a separate managed directory")
	}
	remote, err := g.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if remote != g.config.Remote {
		return errors.New("checkout origin differs from configured remote")
	}
	if _, err := g.git(ctx, "check-ref-format", "refs/heads/"+g.config.Branch); err != nil {
		return err
	}
	return g.fetch(ctx)
}
func (g checkout) fetch(ctx context.Context) error {
	_, err := g.git(ctx, "fetch", "--tags", "--prune", "origin", "+refs/heads/"+g.config.Branch+":"+g.branchRef())
	return err
}
func (g checkout) resolve(ctx context.Context, ref string) (string, error) {
	return g.git(ctx, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}
func (g checkout) contains(ctx context.Context, descendant, ancestor string) error {
	_, err := g.git(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return fmt.Errorf("commit %s is not included in %s: %w", ancestor, descendant, err)
	}
	return nil
}

// releaseHead includes committed work left in the bot's checkout. Counting only
// origin would hide those commits forever when the remote is already released.
// A topic branch or detached HEAD is valid. Preparation reconciles divergence
// through its PR, rather than resetting local work or blocking before the agent.
func (g checkout) releaseHead(ctx context.Context) (string, error) {
	head, err := g.resolve(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	remote, err := g.resolve(ctx, g.branchRef())
	if err != nil {
		return "", err
	}
	if g.contains(ctx, remote, head) == nil {
		return remote, nil
	}
	return head, nil
}

func (g checkout) advance(ctx context.Context) error {
	status, err := g.git(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("checkout has unfinished edits; refusing to overwrite them")
	}
	head, err := g.releaseHead(ctx)
	if err != nil {
		return err
	}
	// Move only this worktree's HEAD/index. Never reset or advance a local
	// master/topic branch that another worktree might be using.
	_, err = g.git(ctx, "switch", "--detach", head)
	return err
}

func (g checkout) startBranch(ctx context.Context) (string, error) {
	branch := "brb/release-" + strings.ToLower(rand.Text())
	_, err := g.git(ctx, "switch", "--no-track", "-c", branch)
	return branch, err
}

func (g checkout) changes(ctx context.Context, base, head string, since time.Time) (int, int, error) {
	// Count the union: concurrent remote commits must remain visible even
	// when a local preparation branch has diverged from the watched branch.
	revisions := []string{head, g.branchRef()}
	if base != "" {
		if err := g.contains(ctx, g.branchRef(), base); err != nil {
			return 0, 0, err
		}
		revisions = append(revisions, "^"+base)
	}
	a, err := g.git(ctx, append([]string{"rev-list", "--count"}, revisions...)...)
	if err != nil {
		return 0, 0, err
	}
	b, err := g.git(ctx, append([]string{"rev-list", "--count", "--since=" + since.Format(time.RFC3339)}, revisions...)...)
	if err != nil {
		return 0, 0, err
	}
	total, err := strconv.Atoi(a)
	if err != nil {
		return 0, 0, err
	}
	recent, err := strconv.Atoi(b)
	return total, recent, err
}

var fullHash = regexp.MustCompile(`^([a-f0-9]{40}|[a-f0-9]{64})$`)

func (g checkout) verify(ctx context.Context, target string, r Result) error {
	if !fullHash.MatchString(r.Commit) {
		return errors.New("result requires a full Git commit hash")
	}
	if r.Tag == "" {
		return errors.New("result is missing a release tag")
	}
	if _, err := g.git(ctx, "check-ref-format", "refs/tags/"+r.Tag); err != nil {
		return err
	}
	if err := g.fetch(ctx); err != nil {
		return err
	}
	// Use a separate ref so an unpublished local tag cannot satisfy verification.
	if _, err := g.git(ctx, "fetch", "--no-tags", "origin", "+refs/tags/"+r.Tag+":refs/release-bot/publication"); err != nil {
		return err
	}
	sha, err := g.resolve(ctx, "refs/release-bot/publication")
	if err != nil {
		return err
	}
	if sha != r.Commit {
		return fmt.Errorf("remote tag is at %s, result claims %s", sha, r.Commit)
	}
	if err := g.contains(ctx, r.Commit, target); err != nil {
		return err
	}
	return g.contains(ctx, g.branchRef(), r.Commit)
}
