# release-bot

An autonomous release daemon in Go. It monitors one repository, drives a configurable coding agent over Agent Client Protocol (ACP), and verifies publication before recording a successful release. Codex through `codex-acp` is the default agent.

The Go code uses only the standard library. The `acp` package is a new implementation of the [ACP v1 specification](https://agentclientprotocol.com/protocol/v1/overview), with no third-party SDK or generated SDK code. This project has no HTTP server.

## Run

Start it in the repository you want released:

```sh
cd /path/to/your-repo
release-bot
```

No configuration file is needed. The bot detects `origin` (or the only remote) and the repository's default branch, sets up its own checkout, and starts checking for releases. It works from subdirectories too. The agent reads the repository's instructions and discovers release workflows, destinations and publishability checks. Your existing working tree and uncommitted edits are left alone; releases use the remote branch's committed history.

You can also point it at a repository directly:

```sh
release-bot /path/to/repo
release-bot https://github.com/OWNER/REPO.git
release-bot --branch main --once
release-bot --agent your-acp-agent --agent-arg=--stdio
release-bot status
```

`--once` performs one scheduled check or recovery attempt and exits. It can publish a release. `--once --force` skips cadence checks but still requires new commits. `status` shows progress without launching an agent. `retry` resets the pending release's attempt budget and resumes work; stop an already-running daemon before using it. Existing `run` and `once` subcommands also work. Flags can appear before or after the repository argument. Use `--help` for options.

The default workspace lives under `$XDG_STATE_HOME/release-bot` or `~/.local/state/release-bot`, with separate checkout/state directories keyed by remote and branch. Restarting the same command reuses them. Both directories are locked against concurrent local processes, and the OS releases locks after crashes. Run only one bot per remote/branch across machines; there is no distributed lock.

## Install and optional configuration

From this source checkout, run `go install ./cmd/release-bot` (Go 1.27.1), or `make build` to produce `bin/release-bot`. Runtime requirements are Linux/macOS, Git, Codex (or another authenticated ACP agent), and `gh` for GitHub repositories. Existing agent, Git and registry credentials are used. If `codex-acp` is installed, the bot uses it. Otherwise it automatically launches the maintained [Codex ACP adapter](https://github.com/agentclientprotocol/codex-acp) through `npx --yes @agentclientprotocol/codex-acp`; Node.js must be installed and the first launch may download the adapter.

Advanced settings are optional: `release-bot --config /path/to/release-bot.json` uses an explicit configuration, with paths relative to that file. The [example](release-bot.example.json) shows available settings. A config file is not auto-created or implicitly loaded. Use `--json` for machine-readable logs and status. Explicit agent commands are used exactly as configured.

## Cadence and recovery

By default the bot polls every five minutes and aims to release every 24 hours when there are unreleased commits. It may release earlier after 20 unreleased commits within two hours, provided two hours have elapsed since the last successful release and the branch has been quiet for 15 minutes. The daily deadline ignores the quiet period so continuous commits cannot starve releases. Set `burst` to zero to disable earlier releases.

On first startup, GitHub repositories use the most recently published release whose tag is reachable from the watched branch as their baseline. Existing release records are trusted for this initial baseline; an arbitrary local tag is not used. `initial_ref` explicitly overrides the baseline. Without an existing release or an explicit baseline, all commits are unreleased and the first check is immediately eligible. Non-GitHub repositories can set `initial_ref` to a known released commit/tag.

A release job records its target before starting the agent. Failed jobs preserve local work and failure evidence, then retry after 15 minutes. Each attempt has a two-hour budget shared by preparation, publishability validation, and publication. After three attempts, the job remains pending for operator inspection and `retry`. A stored publication receipt is rechecked before asking the agent to act again, including after the attempt budget is exhausted. This can reconcile a release whose asynchronous checks eventually pass.

Recovery instructions tell the agent to inspect existing tags, workflow runs and published artifacts before taking action. There is no atomic transaction spanning Git and external registries, so exactly-once publication cannot be guaranteed after a crash before the agent returns its receipt. Reconciliation reduces this risk. The baseline advances only to the verified tagged commit; later commits remain eligible for the next release.

## Publishability before publication

Every attempt starts with a separate preparation session using the embedded [preflight skill](skills/preflight.md). That session can prepare code and validation infrastructure, but its instructions prohibit tags, public releases and registry uploads. It must enumerate every intended publication destination and return a plan containing:

- The exact prepared commit and proposed tag.
- Each destination's version and actual publishing identity/environment.
- Reproducible non-publishing commands checking build/package validity, version availability and publishing authorization for each destination.
- A post-publication verification command for each destination.

The daemon checks the plan's completeness, requires a clean checkout at the prepared commit, runs every preflight command itself, and checks the tree again. Only then does it start a separate publication session. A failed command or missing check prevents that session from starting. Publication must report the approved commit and tag. Each new attempt repeats preparation and validation; a fix that changes the plan requires new preflight checks.

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

```sh
make check
```

The ACP implementation includes newline JSON-RPC framing, bidirectional requests, ordered notifications, request errors and cancellation, initialization/version checks, session creation, optional authentication/modes, prompt completion, permission decisions, file reads/writes, and the full terminal lifecycle. Generic `Call`/`Notify` methods allow extension methods. Optional session history, MCP configuration, elicitation and v2 are not advertised. Protocol references are recorded in [docs/protocol.md](docs/protocol.md).

Tests use in-memory protocol peers, real subprocess pipes, temporary Git remotes and simulated registry checks. They cover permission failures, incomplete plans, partial publication, recovery, concurrent commits, scheduling, locking, path confinement and cancellation. They do not exercise real Codex credentials or publish to live registries.

Licensed under [Apache License 2.0](LICENSE). The license text was obtained unmodified from the Apache Software Foundation's license endpoint.
