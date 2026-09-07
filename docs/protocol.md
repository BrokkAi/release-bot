# Protocol sources and implementation

The ACP client, now maintained in [BrokkAi/acp-go](https://github.com/BrokkAi/acp-go), was written against the protocol documentation, using Go's standard library. It does not import, vendor, generate from, or copy an existing ACP SDK implementation. Protocol field names, method names and wire values necessarily match the specification.

Sources consulted on 2026-09-07:

- https://agentclientprotocol.com/protocol/v1/initialization
- https://agentclientprotocol.com/protocol/v1/authentication
- https://agentclientprotocol.com/protocol/v1/session-setup
- https://agentclientprotocol.com/protocol/v1/session-config-options
- https://agentclientprotocol.com/protocol/v1/prompt-turn
- https://agentclientprotocol.com/protocol/v1/transports
- https://agentclientprotocol.com/protocol/v1/cancellation
- https://agentclientprotocol.com/protocol/v1/tool-calls
- https://agentclientprotocol.com/protocol/v1/file-system
- https://agentclientprotocol.com/protocol/v1/terminals

`acp.Connection` owns the input/output streams. It processes notifications in receive order before delivering subsequent responses, while requests are served concurrently so terminal waits do not stop other protocol traffic. Notification callbacks must be quick and must not call into the same connection. Request handlers must honor context cancellation. Closing the connection closes the streams and cancels and joins its handlers. Messages above 8 MiB and more than 32 simultaneous incoming requests are rejected by closing the connection.

GitHub CLI and REST references:

- https://cli.github.com/manual/gh_api
- https://cli.github.com/manual/gh_run_list
- https://cli.github.com/manual/gh_run_view
- https://cli.github.com/manual/gh_run_watch
- https://docs.github.com/en/rest/actions/workflow-runs
- https://docs.github.com/en/rest/releases/releases

Project license source: https://www.apache.org/licenses/LICENSE-2.0.txt

Crates.io/Cargo preflight references:

- https://doc.rust-lang.org/cargo/commands/cargo-publish.html
- https://doc.rust-lang.org/cargo/reference/publishing.html
