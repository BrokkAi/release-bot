# Brokk Release Bot

An autonomous release daemon in Go. It monitors one repository, drives a configurable coding agent over Agent Client Protocol (ACP), and verifies publication before recording a successful release. Codex through `codex-acp` is the default agent.

The bot uses the shared [acp-go](https://github.com/BrokkAi/acp-go) module for ACP v1 transport, process/session lifecycle, client filesystem and terminal tools, transcripts, and model/effort selection. The library was extracted from this project and is also used by [issue-bot](https://github.com/BrokkAi/issue-bot). It uses only Go's standard library, with no third-party SDK or generated SDK code. This project has no HTTP server.

## Install

The command is **`brb`**. Choose one installation method; supported platforms are Linux and macOS on amd64 and arm64.

**Go** (Go 1.27.1 or newer):

```sh
go install github.com/BrokkAi/release-bot/cmd/brb@latest
```

Go installs `brb` into `GOBIN`, or `$(go env GOPATH)/bin` by default. Add that directory to your `PATH`. Replace `@latest` with a release tag such as `@v0.2.0` to pin a version. See the [Go installation reference](https://go.dev/ref/mod#go-install). From a source checkout, use `go install ./cmd/brb`, or `make build` to produce `bin/brb`.

**curl** (prebuilt binary, no Go required):

```sh
curl -fsSL https://raw.githubusercontent.com/BrokkAi/release-bot/master/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
```

The installer downloads the latest stable GitHub release, verifies its SHA-256 checksum, and installs `brb` in `~/.local/bin`. Add the PATH line to your shell profile to keep it across sessions. Rerun to upgrade. To select a release and destination:

```sh
curl -fsSL https://raw.githubusercontent.com/BrokkAi/release-bot/master/install.sh | INSTALL_DIR="$HOME/.local/bin" sh -s -- v0.2.0
```

**npm** (Node.js 18 or newer; [published package](https://www.npmjs.com/package/@brokkai/release-bot)):

```sh
npm install -g @brokkai/release-bot
brb --help
```

The npm package installs the matching native binary through an optional platform dependency. Keep optional dependencies enabled. For a one-off invocation, use `npx --yes @brokkai/release-bot --help`. Rerun the install command with `@latest` to upgrade, or append a version such as `@0.2.0` to pin it.

**uv** (Python 3.10 or newer; PyPI publication pending):

Use Go, curl, or npm for now. Once the first PyPI release is published:

```sh
uv tool install brokk-release-bot
brb --help
```

For a one-off invocation, use `uvx --from brokk-release-bot brb --help`. The Python package downloads its exact native release on first use and checks archive and binary hashes embedded in the package. Later launches use the verified cache under `$XDG_CACHE_HOME/brokk-release-bot` or `~/.cache/brokk-release-bot`; `BROKK_RELEASE_BOT_CACHE_DIR` overrides it. Use `uv tool upgrade brokk-release-bot` to upgrade, or install `brokk-release-bot==0.2.0` to pin it. Run `uv tool update-shell` if the tool directory is missing from your PATH.

Go, curl, and npm installation are available now. See [RELEASING.md](RELEASING.md) for release procedures, npm trusted publishing, and the remaining PyPI publisher setup.

## Run

Start it in the repository you want released:

```sh
cd /path/to/your-repo
brb
```

No configuration file is needed. The bot detects `origin` (or the only remote) and the repository's default branch, sets up its own checkout, and starts checking for releases. It works from subdirectories too. The agent reads the repository's instructions and discovers release workflows, destinations and publishability checks. Your existing working tree and uncommitted edits are left alone; releases use the remote branch's committed history.

If the repository has no release process, the agent is instructed to create it: build/package scripts, GitHub CI and release workflows, publishability checks, and `RELEASING.md`. It commits the setup, runs non-publishing validation, repairs failures, and then attempts the first release. Missing credentials or account permissions produce a specific blocked report before publication; missing workflows alone do not require manual setup.

You can also point it at a repository directly:

```sh
brb /path/to/repo
brb https://github.com/OWNER/REPO.git
brb --branch main --once
brb --agent your-acp-agent --agent-arg=--stdio
brb --model YOUR_MODEL_ID
brb --model YOUR_MODEL_ID --effort low
brb status
```

`--once` performs one scheduled check or recovery attempt and exits. It can publish a release. `--once --force` skips cadence checks but still requires new commits. `status` shows progress without launching an agent. `retry` resets the pending release's attempt budget and resumes work; stop an already-running daemon before using it. Existing `run` and `once` subcommands also work. Flags can appear before or after the repository argument. Use `--help` for options.

The default workspace lives under `$XDG_STATE_HOME/release-bot` or `~/.local/state/release-bot`, with separate checkout/state directories keyed by remote and branch. New workspaces use a Git worktree backed by the bot's own bare repository at `state/repository.git`. Your checkout and other bots' worktrees do not share its index, local branches, tags or Git configuration. Existing managed clones continue working in place, including unfinished jobs.

Each new release starts on a unique `brb/release-...` branch. The preparation agent makes its fixes there, opens or reuses that job's PR, incorporates concurrent changes from the watched `master`/`main` branch, resolves conflicts, and follows the PR through merge. It then checks out the exact merged commit in its private worktree for validation and publication. Changes other bots push during publication remain eligible for the next release. The bot never resets another checkout or another bot's branch. Overlapping code changes can still require PR conflict resolution or human review; workspace isolation prevents local interference.

You can leave `brb` running in the background while other bots work. Restarting reuses the workspace and pending PR/job, including unfinished edits. The checkout and state directories are locked against duplicate local daemons, and the OS releases locks after crashes. Run only one release bot per remote/branch across machines; there is no distributed lock. A manually configured directory must be a standalone clone or this bot's private worktree, not a linked worktree sharing another repository's Git metadata.

## Runtime requirements and optional configuration

Runtime requirements are Linux/macOS, Git, Codex (or another authenticated ACP agent), and `gh` for GitHub repositories. Existing agent, Git and registry credentials are used. If `codex-acp` is installed, the bot uses it. Otherwise it automatically launches the maintained [Codex ACP adapter](https://github.com/agentclientprotocol/codex-acp) through `npx --yes @agentclientprotocol/codex-acp`; Node.js must be installed and the first launch may download the adapter.

Advanced settings are optional: `brb --config /path/to/release-bot.json` uses an explicit configuration, with paths relative to that file. The [example](release-bot.example.json) shows available settings. A config file is not auto-created or implicitly loaded. Use `--json` for machine-readable logs and status. Explicit agent commands are used exactly as configured.

Select a model with a flag:

```sh
brb --model YOUR_MODEL_ID
```

The flag works with Codex and other agents that advertise ACP model selection. The bot selects and confirms it before sending each preparation or publication prompt. Unknown model IDs report the agent's available choices; unsupported selection fails explicitly. Without the flag, the agent's default applies. An optional `agent.model` config value persists the choice; `--model` overrides it.

Set reasoning effort with `--effort low` (or another value advertised by your agent, such as `medium` or `high`). It can be used with or without `--model`. The bot selects effort after the model and confirms it before every preparation or publication prompt. Unsupported effort values report the available choices; agents without effort selection fail explicitly. Omitting the option keeps the agent's configured default. Set `"effort": "low"` inside the config's `agent` object to persist it; `--effort` overrides that value.

## Cadence and recovery

By default the bot polls every five minutes and aims to release every 24 hours when there are unreleased commits. It may release earlier after 5 unreleased commits within two hours, provided two hours have elapsed since the last successful release and the branch has been quiet for 15 minutes. The daily deadline ignores the quiet period so continuous commits cannot starve releases. Override the commit threshold at startup with `--burst 3`, for example `./bin/brb --model gpt-5.6-luna --effort xhigh --burst 3`. The flag overrides `burst` in a configuration file; omitting it preserves the configured value. Use `--burst 0` (or configure `burst` as zero) to disable earlier releases.

Adjust the early-release delays with `--minimum-gap 10m --quiet 1m`. Both accept durations such as `30s`, `5m`, or `1h` and override the corresponding configuration values (`minimum_gap` and `quiet`). Set either to `0` to disable that delay. Omitting the flags preserves configured values, or the defaults of two hours and 15 minutes. The commit threshold, burst window, and polling interval still apply.

Startup checks immediately: if the last release is at least `daily` old (24 hours by default), or there has never been a release, the bot starts release preparation on that first check. It does not wait for a polling interval, a quiet period, or another day after startup. Restarting does not reset the deadline. Existing releases still require unreleased commits, and every attempt must pass publishability checks before publishing.

On first startup, GitHub repositories use the most recently published release whose tag is reachable from the watched branch as their baseline. Existing release records are trusted for this initial baseline; an arbitrary local tag is not used. `initial_ref` explicitly overrides the baseline. Without an existing release or an explicit baseline, all commits are unreleased and the first check is immediately eligible. Non-GitHub repositories can set `initial_ref` to a known released commit/tag.

A release job records its target before starting the agent. Failed jobs preserve local work and failure evidence, then retry after 15 minutes. Each attempt has a two-hour budget shared by preparation, publishability validation, and publication. After three failed release attempts, the daemon exits with the final failure and leaves the job pending for operator inspection and `retry`. Restarting still reconciles saved publication evidence before enforcing the exhausted budget. A stored publication receipt is rechecked before asking the agent to act again, including after the attempt budget is exhausted. If the publisher lost its connection before returning a receipt, the daemon first tries to verify publication directly from the saved publication plan. This can reconcile a release whose asynchronous checks eventually pass.

Recovery instructions tell the agent to inspect existing tags, workflow runs and published artifacts before taking action. There is no atomic transaction spanning Git and external registries, so exactly-once publication cannot be guaranteed after a crash before the agent returns its receipt. Reconciliation reduces this risk. The baseline advances only to the verified tagged commit; later commits remain eligible for the next release.

Unpushed commits in the bot's managed checkout also count as unreleased work, including on topic branches or a detached HEAD. Polling counts both local and remote commits when their histories diverge. When the normal release cadence is due, preparation preserves those commits and, on GitHub, opens or reuses a PR, fixes its checks, and follows it through merge before preparing a new version. The publication gate requires the prepared commit to be on the remote release branch; squash and rebase merges use the resulting commit. Required human approvals remain blockers until satisfied. The bot never force-pushes or overwrites divergent history or unfinished edits. Commits in a separate development checkout are not imported automatically.

Restarting immediately resumes a pending job, even if its saved retry timer has not elapsed. The failure budget still applies to actual failures. Ctrl+C or SIGTERM preserves unfinished work, records the stop reason, and does not consume an attempt or impose a retry delay. Ordinary polling after an actual failure retains the configured backoff until the attempt limit is reached.

Agent setup failures (including an unknown model or unsupported effort) exit immediately before any prompt and do not consume release attempts or impose a retry delay. Correct the command-line/config setting and restart normally; the new invocation supplies the agent settings. Setup errors are recorded separately from release failures. For state saved by older versions, startup recognizes the old unknown-model/effort error and refunds that last setup attempt once, allowing corrected settings to run even if the saved budget was exhausted. Earlier release failures remain counted.

## Publishability before publication

A new release starts with a separate preparation session using the embedded [preflight skill](skills/preflight.md). That session can prepare code and validation infrastructure, but its instructions prohibit tags, public releases and registry uploads. It must enumerate every intended publication destination and return a plan containing:

- The exact prepared commit and proposed tag.
- Each destination's version and actual publishing identity/environment.
- Reproducible non-publishing commands checking build/package validity, version availability and publishing authorization for each destination.
- A post-publication verification command for each destination.

The daemon checks the plan's completeness, requires a clean checkout at the prepared commit, runs the preflight commands itself, and checks the tree again. Successful build checks are saved individually, so an interruption during a later check preserves completed work. Only then does it start a separate publication session. A failed command or missing check prevents publication. Publication must report the approved commit and tag.

Retries reuse the saved plan when the checkout is clean at the planned commit and no gate failure or blocked publication requires preparation repairs. Successful build checks are reused only for the identical plan, target and check; changed commands, destinations or commits invalidate that evidence, as does an observed dirty checkout. Authorization, version availability, other check kinds and the operator preflight always run again before another publication session. Keep mutable prerequisites out of build checks. Cached build success establishes validation, not the continued presence of local build outputs; the publication procedure must produce missing artifacts as needed. Older state files without checkpoints run the checks once to establish them.

The publication agent receives the plan and the time the daemon completed its gate. Its instructions limit it to reconciliation, publication and a prompt receipt, using the existing validation evidence. Code or workflow repairs return to preparation with the specific failure. The daemon still independently verifies all published destinations. GitHub waiting instructions use bounded polling with status changes and occasional heartbeats instead of repeatedly printing the full job table.

For crates.io, `cargo publish --dry-run` checks packaging; it is not evidence of remote publish authorization. The skill requires separate checks of the actual publisher's ownership, token scope/expiry, or trusted-publisher configuration. When publishing through GitHub Actions, local credentials and secret names are insufficient: the checks must establish rights in the publishing workflow's environment. Unknown rights must block publication. The same principle applies to npm, PyPI, containers, signing and other destinations.

The agent discovers the repository-specific checks. Their correctness and destination coverage depend on the repository procedure and available registry capabilities; the daemon does not contain universal registry permission adapters. For an additional operator-maintained gate, configure `preflight` to a verifier outside the checkout. It runs after the destination checks, with `RELEASE_COMMIT`, `RELEASE_TAG`, `RELEASE_TARGET` and `RELEASE_PLAN_JSON` in the environment, and must exit nonzero if any destination is not demonstrably publishable.

```json
"preflight": ["/opt/release-checks/publishability"]
```

After publishing, every destination's verification command must pass as well as the built-in GitHub checks and optional operator verifier. Missing registry artifacts keep the job pending even if its GitHub release exists. Preflight prevents foreseeable partial releases; network failures and non-transactional registries can still fail mid-publication. The publication skill directs the agent to stage privately where possible, publish the final release/announcement last, and reconcile partial artifacts on retry. The phase boundary is an agent instruction and daemon orchestration rule, not an OS/network sandbox.

## GitHub is built in

The [GitHub skill](skills/github.md) is embedded into every GitHub release prompt for every configured agent. It covers repository/workflow discovery, `gh run list`, failed job logs, reruns, dispatch, PR checks, publication and assets. The [release skill](skills/release.md) instructs the agent to follow repository instructions, repair failures and finish the release.

The daemon also independently verifies with `gh api`:

- The remote tag points to the reported full commit, includes the job target, and is reachable from the watched branch.
- Actions runs belong to that exact tagged commit. The latest run/attempt in every observed workflow/event/ref context must finish with `success`.
- Every required workflow has a run. The agent discovers workflow names/paths from the repository and records them in the publication plan; the daemon requires this list when none is configured. Optional `github.workflows` entries add operator requirements. Entries match workflow names, paths, or filenames.
- A published, non-draft GitHub release exists for that tag. Uploaded assets must be nonempty and complete. Every `github.assets` glob must match at least one asset.

Pending or missing runs are polled within `verification_timeout`; failed, cancelled or skipped runs cause repair feedback. A passing branch run cannot mask a failed tag run. The verifier paginates API results and refuses incomplete histories. Destination-specific commands in the validated plan check package registries and artifact contents; an optional operator `verify` command can enforce additional requirements.

GitHub HTTPS/SSH remotes are detected automatically. For an enterprise host set `github.host`; for a local mirror set `github.repo` to `OWNER/REPO`. The `gh` process uses its own inherited authentication, independent of the agent's environment overrides.

```json
"github": {
  "host": "github.com",
  "workflows": ["ci.yml", "release.yml"],
  "assets": ["*-linux-amd64.tar.gz", "checksums.txt"]
}
```

## Other agents and release destinations

Set `agent.command` to an executable and argument array. `agent.environment` supplies additional environment variables, `agent.auth_method` optionally selects an advertised protocol-driven login method, and `agent.mode` optionally selects a session mode. Authenticate interactive agents before starting the daemon. ACP v1 stdio agents with their own tools or client filesystem/terminal tools are supported; draft ACP v2 is not supported.

```json
"agent": {
  "command": ["your-acp-agent", "--stdio"],
  "environment": {"YOUR_AGENT_SETTING": "value"}
}
```

`verify` is an optional extra command for any Git host. The agent already supplies mandatory destination-specific verification commands in its plan. An operator verifier runs directly as an argument array, in the checkout, with `RELEASE_TAG`, `RELEASE_COMMIT`, `RELEASE_TARGET`, `RELEASE_URL`, `RELEASE_REMOTE`, and `RELEASE_BRANCH` in its environment. Exit zero only when the exact release's checks, artifacts and registry publication have succeeded. Keep the verifier outside the agent's writable checkout. There is no shell expansion unless you explicitly configure a shell.

```json
"verify": ["/opt/release-checks/verify-publication"]
```

## Execution and diagnostics

Starting the bot authorizes unattended code edits, command execution, commits, pushes and publication for the configured repository. ACP permission requests are approved automatically and recorded. Instructions require focused fixes and respect for branch protections. Client file operations use `os.Root` to prevent paths and symlinks escaping the checkout. Shell commands and the external agent run with the bot account's OS permissions: this is not a sandbox. Run it as a dedicated account or in a container with the credentials and tools required for that repository.

Readable progress goes to stderr; `--json` selects structured logs. Private JSONL session transcripts, including tool updates and agent diagnostics, live under `state_directory/sessions`. The workspace location is printed at startup. State writes use fsync and atomic replacement. Retain state across deployments. Configure log retention externally; transcripts may include repository contents or command output.

## Development and protocol scope

The live transcript is shown by default: agent messages and thought updates, tool output, completion/failure status, and agent stderr appear as they arrive. Text fragments are joined into readable lines. `--json` keeps these as structured stream events. Session transcripts also include a `session_end` record with the phase, error, context cancellation cause (when available), and transport failure, so a future interruption can be diagnosed from disk.

This repository's [release instructions](RELEASING.md) describe its CI and release workflows. CI runs on pushes and pull requests; the release workflow builds Linux/macOS archives, checks publisher access, and verifies staged assets before publication.

```sh
make check
```

To test your real ACP adapter and model with a read-only fixture, without running a release:

```sh
RELEASE_BOT_LIVE_SMOKE=1 RELEASE_BOT_LIVE_MODEL=gpt-5.6-sol go test -run '^TestLiveACP$' -v .
```

The ACP implementation includes newline JSON-RPC framing, bidirectional requests, ordered notifications, request errors and cancellation, initialization/version checks, session creation, optional authentication/modes, prompt completion, permission decisions, file reads/writes, and the full terminal lifecycle. Generic `Call`/`Notify` methods allow extension methods. Optional session history, MCP configuration, elicitation and v2 are not advertised. Protocol references are recorded in [docs/protocol.md](docs/protocol.md).

Tests use in-memory protocol peers, real subprocess pipes, temporary Git remotes and simulated registry checks. They cover permission failures, incomplete plans, partial publication, recovery, concurrent commits, scheduling, locking, path confinement and cancellation. They do not exercise real Codex credentials or publish to live registries.

Licensed under [Apache License 2.0](LICENSE). The license text was obtained unmodified from the Apache Software Foundation's license endpoint.

## Automatic releases of this project

Pushing a new version tag starts the complete **Publish packages** workflow:
CI and native GitHub publication, followed automatically by all five npm packages
at the same tag and commit. No separate package dispatch is needed. Branch
pushes do not publish. PyPI remains an explicit manual option until configured.

Native checksums, local installer tests, package hashes and upload errors remain
release gates. Successful npm uploads do not wait for the public version index or
run immediate public-install checks. Manual package dispatch and the explicit
registry verification command remain available for recovery and later checks.
See [RELEASING.md](RELEASING.md).
