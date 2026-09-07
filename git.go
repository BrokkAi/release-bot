package releasebot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/BrokkAi/release-bot/internal/osrun"
)

type checkout struct{ config Config }

func (g checkout) git(ctx context.Context, args ...string) (string, error) {
	return osrun.Run(ctx, g.config.Directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"}, append([]string{"git"}, args...)...)
}
func (g checkout) branchRef() string { return "refs/remotes/origin/" + g.config.Branch }
func (g checkout) open(ctx context.Context) error {
	if _, err := os.Stat(g.config.Directory); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(g.config.Directory), 0700); err != nil {
			return err
		}
		_, err = osrun.Run(ctx, "", map[string]string{"GIT_TERMINAL_PROMPT": "0"}, "git", "clone", "--branch", g.config.Branch, "--origin", "origin", "--", g.config.Remote, g.config.Directory)
		if err != nil {
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
		return fmt.Errorf("directory must be the root of its own clone: %s", root)
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
// Preparation owns pushing them after inspecting the repository's push triggers.
func (g checkout) releaseHead(ctx context.Context) (string, error) {
	branch, err := g.git(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if branch != g.config.Branch {
		return "", fmt.Errorf("checkout must be on %s, currently %s", g.config.Branch, branch)
	}
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
	if g.contains(ctx, head, remote) == nil {
		return head, nil
	}
	return "", errors.New("checkout and remote have divergent commits; reconcile them without discarding local work before a new release")
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
	_, err = g.git(ctx, "merge", "--ff-only", head)
	return err
}
func (g checkout) changes(ctx context.Context, base, head string, since time.Time) (int, int, error) {
	revision := head
	if base != "" {
		if err := g.contains(ctx, head, base); err != nil {
			return 0, 0, err
		}
		revision = base + ".." + head
	}
	a, err := g.git(ctx, "rev-list", "--count", revision)
	if err != nil {
		return 0, 0, err
	}
	b, err := g.git(ctx, "rev-list", "--count", "--since="+since.Format(time.RFC3339), revision)
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
