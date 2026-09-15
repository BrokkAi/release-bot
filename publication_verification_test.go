package releasebot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationRecoveryUsesReleasedTree(t *testing.T) {
	for _, workspace := range []string{"dirty", "wrong-revision"} {
		for _, operator := range []bool{false, true} {
			for _, receipt := range []bool{false, true} {
				for _, exhausted := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/operator=%t/receipt=%t/exhausted=%t", workspace, operator, receipt, exhausted), func(t *testing.T) {
						f := newFixture(t)
						ctx := context.Background()
						if err := f.engine.git.open(ctx); err != nil {
							t.Fatal(err)
						}
						dir := f.engine.config.Directory
						script := filepath.Join(dir, "verify-release.sh")
						writeTestFile(t, script, "#!/bin/sh\ntest -f \"$1\"\n")
						localGit(t, dir, "add", "verify-release.sh")
						localGit(t, dir, "commit", "-m", "add publication verifier")
						localGit(t, dir, "push", "origin", "HEAD:master")
						f.head = localGit(t, dir, "rev-parse", "HEAD")
						r := f.published(t, false)
						r.Plan = f.plan()
						command := []string{"sh", "verify-release.sh", f.registry}
						if operator {
							r.Plan.Destinations[0].Verify = []string{"git", "cat-file", "-e", f.head}
							f.engine.config.Verify = command
						} else {
							r.Plan.Destinations[0].Verify = command
						}
						if err := f.engine.verify(ctx, f.head, r); err == nil {
							t.Fatal("fixture must reject missing artifact")
						}
						localGit(t, dir, "switch", "-c", "unfinished-repair")
						writeTestFile(t, script, "#!/bin/sh\nexit 0\n")
						localGit(t, dir, "add", "verify-release.sh")
						if workspace == "wrong-revision" {
							localGit(t, dir, "commit", "-m", "unfinished verifier repair")
						} else {
							writeTestFile(t, script, "#!/bin/sh\n# unfinished edits\nexit 0\n")
							writeTestFile(t, filepath.Join(dir, "repair-notes"), "preserve me")
						}
						snapshot := func() string {
							data, err := os.ReadFile(script)
							if err != nil {
								t.Fatal(err)
							}
							return localGit(t, dir, "rev-parse", "HEAD") + localGit(t, dir, "status", "--porcelain") + localGit(t, dir, "diff", "--cached") + localGit(t, dir, "diff") + localGit(t, dir, "worktree", "list", "--porcelain") + string(data)
						}
						before := snapshot()
						j := &Job{Target: f.head, Plan: r.Plan}
						if receipt {
							j.Candidate = &r
						}
						if exhausted {
							j.Tries = f.engine.config.Attempts
						}
						s := &State{Format: 1, Remote: f.engine.config.Remote, Branch: f.engine.config.Branch, Directory: dir, Job: j}
						if err := f.engine.save(s); err != nil {
							t.Fatal(err)
						}
						f.engine.starting = true
						f.engine.agent = scriptedAgent(func(context.Context, string) (Result, error) {
							return Result{}, errors.New("fixture repair remains unfinished")
						})
						if err := f.engine.cycle(ctx, false); err == nil {
							t.Fatal("missing artifact marked released")
						}
						s = fixtureState(t, f)
						if s.Job == nil || s.Released == f.head {
							t.Fatal("incomplete publication did not stay pending")
						}
						if snapshot() != before {
							t.Fatal("verification changed repair workspace or leaked a worktree")
						}
						// Reconciliation must still succeed after the budget is exhausted when
						// the exact released script finally confirms asynchronous publication.
						s.Job.Tries = f.engine.config.Attempts
						if err := f.engine.save(s); err != nil {
							t.Fatal(err)
						}
						writeTestFile(t, f.registry, "published")
						if err := f.engine.cycle(ctx, false); err != nil {
							t.Fatal(err)
						}
						s = fixtureState(t, f)
						if s.Job != nil || s.Released != f.head {
							t.Fatal("complete publication was not reconciled")
						}
						if snapshot() != before {
							t.Fatal("successful verification changed repair workspace")
						}
					})
				}
			}
		}
	}
}

func TestPublicationVerificationRejectsTreeChanges(t *testing.T) {
	for _, mutation := range []string{"echo changed > manifest", "echo staged > manifest; git add manifest", "git checkout --detach HEAD^", "echo untracked > unexpected"} {
		for _, operator := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/operator=%t", mutation, operator), func(t *testing.T) {
				f := newFixture(t)
				ctx := context.Background()
				if err := f.engine.git.open(ctx); err != nil {
					t.Fatal(err)
				}
				r := f.published(t, true)
				r.Plan = f.plan()
				command := []string{"sh", "-c", mutation}
				marker := filepath.Join(t.TempDir(), "next-command")
				if operator {
					f.engine.config.Verify = command
				} else {
					r.Plan.Destinations[0].Verify = command
					f.engine.config.Verify = []string{"touch", marker}
				}
				before := localGit(t, f.engine.config.Directory, "worktree", "list", "--porcelain")
				if err := f.engine.verify(ctx, f.head, r); err == nil || !strings.Contains(err.Error(), "verification tree") {
					t.Fatalf("tree mutation accepted: %v", err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("ran another verifier after tree mutation")
				}
				if after := localGit(t, f.engine.config.Directory, "worktree", "list", "--porcelain"); after != before {
					t.Fatal("temporary worktree leaked")
				}
				if localGit(t, f.engine.config.Directory, "status", "--porcelain") != "" || localGit(t, f.engine.config.Directory, "rev-parse", "HEAD") != f.head {
					t.Fatal("verifier modified repair workspace")
				}
			})
		}
	}
}

func TestPublicationVerificationChecksInitialTree(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.published(t, true)
	r.Plan = f.plan()
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "post-checkout")
	writeTestFile(t, hook, "#!/bin/sh\necho hook-edited > manifest\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	localGit(t, f.engine.config.Directory, "config", "core.hooksPath", hooks)
	marker := filepath.Join(t.TempDir(), "verifier-ran")
	r.Plan.Destinations[0].Verify = []string{"touch", marker}
	if err := f.engine.verify(ctx, f.head, r); err == nil || !strings.Contains(err.Error(), "verification tree") {
		t.Fatalf("initial dirty tree accepted: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verifier ran in initially dirty tree")
	}
}

func TestPublicationVerificationInitializesReleasedSubmodules(t *testing.T) {
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	module := t.TempDir()
	localGit(t, module, "init")
	writeTestFile(t, filepath.Join(module, "verify.sh"), "#!/bin/sh\ntest -f \"$1\"\n")
	localGit(t, module, "add", "verify.sh")
	localGit(t, module, "commit", "-m", "add verifier")
	dir := f.engine.config.Directory
	localGit(t, dir, "submodule", "add", module, "checks")
	localGit(t, dir, "commit", "-m", "add verification submodule")
	localGit(t, dir, "push", "origin", "HEAD:master")
	f.head = localGit(t, dir, "rev-parse", "HEAD")
	r := f.published(t, true)
	r.Plan = f.plan()
	r.Plan.Destinations[0].Verify = []string{"sh", "checks/verify.sh", f.registry}
	writeTestFile(t, f.registry, "published")
	if err := f.engine.verify(ctx, f.head, r); err != nil {
		t.Fatalf("released submodule verifier failed: %v", err)
	}
}

func TestVerificationTreeCleanupAfterCancellation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy-clone=", legacy), func(t *testing.T) {
			f := newFixture(t)
			if legacy {
				localGit(t, t.TempDir(), "clone", f.engine.config.Remote, f.engine.config.Directory)
			}
			if err := f.engine.git.open(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := localGit(t, f.engine.config.Directory, "worktree", "list", "--porcelain")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := f.engine.git.withVerificationTree(ctx, f.head, func(tree checkout) error {
				if err := tree.checkVerificationTree(ctx, f.head); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(tree.config.Directory, "manifest"), "interrupted output")
				cancel()
				return ctx.Err()
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if after := localGit(t, f.engine.config.Directory, "worktree", "list", "--porcelain"); after != before {
				t.Fatal("canceled verification leaked worktree")
			}
			entries, err := filepath.Glob(filepath.Join(f.engine.config.StateDirectory, "verification-*"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary verification files remain: %v %v", entries, err)
			}
		})
	}
}
