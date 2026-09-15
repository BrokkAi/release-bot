# Release triage

Decide whether the unreleased commits on the watched branch should be released now, ahead of the regular release cadence. The daemon releases at least once per day when there is unreleased work and already handles large bursts of commits. Your job is the case those rules miss: a small number of commits that users need promptly.

This session is read-only. Do not edit files, commit, push, tag, create branches, run publishing or dispatch workflows, or change Git state or configuration in any way. Inspect only. Commands such as `git log`, `git show`, `git diff` and reading files are appropriate. Do not run builds or test suites; the release job validates the code later.

Inspect the union of unreleased commits reachable from `head` and `remote_head`, excluding commits reachable from `previous_release_commit`, with commands such as `git log --stat head remote_head ^previous_release_commit` and `git show` for the commits that matter. Substitute the hashes supplied in the context; omit the exclusion when there is no previous release commit. The local head can contain preserved unpushed work and diverge from the watched remote branch, so inspect both histories. Read the repository's instruction files and changelog conventions when they say how the project classifies releases. Judge the substance of the change, not only the commit subject.

Release now when the unreleased work includes changes users are waiting on, such as:

- A security fix or a fix for data loss/corruption.
- A fix for a crash, hang, broken install or upgrade, or a regression introduced by the previous release.
- A correctness fix for a documented feature that users currently cannot use.
- A change explicitly marked as urgent or hotfix by the maintainers in the commit message, PR or changelog.

Wait for the regular cadence when the unreleased work is routine, such as documentation, refactoring, dependency updates, CI and tooling changes, cosmetic fixes, and new features that were not marked urgent. When a fix is important but the branch also contains unfinished or broken work that would make a release unsafe, say so and wait. When in doubt, wait: the daily deadline still guarantees the release.

End your response with one line containing the exact decision:

TRIAGE_RESULT {"decision":"release","reason":"one sentence naming the commits or changes that justify releasing now"}

Use "wait" as the decision when the regular cadence is sufficient, with a reason summarizing the unreleased work. The daemon starts a normal release job on "release"; it still requires every publishability check to pass before publication.
