# Managed MCP Shutdown

Native Google ADK toolsets and integrated MCP commands use a host-lifetime
`mcp.ClientLifecycle`. The host injects the existing dynamic header provider and
its lifecycle authority provider into both presentations. Standalone commands
still close their SDK session after each command.

The SDK owns initialization, protocol negotiation, session IDs, framing, and
termination requests. Managed transports return its original connection, which
preserves its private negotiated-version callbacks. Native tool conversion,
allow-lists, app classification, and confirmation remain with Google ADK.

## Authority

Current operations resolve headers from the original agent context. A native
request wrapper preserves that context through SDK cancellation wrappers;
connections and long-lived GET streams carry no prior invocation context.
Connection-owned traffic resolves an opaque provider reference instead.
When a Streamable HTTP connection requires dynamic/forwarded credentials but
has no provider custody, its optional standalone GET stream is explicitly
disabled and logged as unavailable. Current invocation calls and their response
streams still authenticate normally. Static and provider-held standalone streams
remain enabled. A legacy SSE connection, whose initial GET is required, fails
explicitly if no current or provider-held authority can authorize it.

For managed Streamable HTTP, the configured request timeout bounds POST requests
through response-body consumption. The standalone GET belongs to the connection
lifetime and is not expired by that request budget. DELETE retains its separate
bounded shutdown deadline. Actual stream or relationship loss remains explicit;
this separation does not enable automatic reconnection or operation replay.

STS references identify existing session/subject cache custody and its generation.
They contain no token or invocation. Resolution cannot exchange a token, renew
expiry or idle eviction, or revive custody after eviction/replacement. Propagate-only
mode uses the same existing cache. Context-only forwarded credentials do not grant
lifecycle authority. Configured static Authorization retains its precedence over
provider credentials; current invocation propagation, allowed headers, dynamic
provider, and static headers retain their existing order.

## Shutdown Evidence

Host shutdown stops admission, cancels pending connections, and closes all owned
SDK connections concurrently. DELETE uses a detached context under a shared
one-second shutdown deadline, then local cancellation and pool release occur
regardless of the peer's response. Explicit `ClientLifecycle.Close` is idempotent
and returns evidence; host cancellation logs the same evidence without credentials
or session identifiers. Standalone commands report abnormal termination evidence
on stderr while preserving successful command results on stdout and keeping
ordinary successful/local-only teardown quiet.

| Status | Evidence |
| --- | --- |
| `accepted` | Peer returns HTTP 2xx to the SDK's DELETE |
| `already_missing` | Peer returns HTTP 404 or 410 |
| `unsupported` | Peer returns HTTP 405 |
| `auth_refused` | Peer returns HTTP 401 or 403 |
| `remote_refused` | Other HTTP refusal |
| `authority_unavailable` | Existing provider custody cannot authorize DELETE |
| `timed_out` | The bounded termination request expires |
| `transport_failed` | A network or local transport failure prevents confirmation |
| `no_session` | SDK has no owned protocol session to terminate |
| `unsupported_transport` | Legacy SSE has only local connection teardown |
| `not_attempted` | No remote termination response is observed |

Every returned outcome also records local release and, when available, the HTTP
status. SDK Close returning nil does not prove remote acceptance. MCP termination
does not establish deletion of browser tabs or other domain resources.

[Scoped client continuity](mcp-client-continuity.md) adds shared caller ownership,
explicit relationship loss, and mutation replay prevention for both presentations.
