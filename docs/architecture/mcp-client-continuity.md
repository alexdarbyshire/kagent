# Scoped MCP Client Continuity

Native tools and integrated commands acquire client relationships from the
host's `mcp.ClientLifecycle`. Each immutable binding is separated by application,
user, native conversation, agent, branch, isolation scope, and effective authority
identity. Invocation IDs are intentionally excluded. Separate command processes
and later native invocations can therefore retain browser ownership without
giving another caller access to it. Native and command presentations use the
same ownership policy; they do not concurrently drive one SDK connection.

Google ADK retains responsibility for native tool conversion, filtering, and
confirmation. A scoped native tool wrapper selects the current caller again at
execution, so retaining a tool returned during another caller's discovery cannot
select that caller's connection. The agent boundary carries the actual invocation
isolation scope as immutable context data because Google's tool context does not
expose it. Neither relationship keys nor transports retain an old invocation.

## Authority And Calls

The existing header resolution order remains propagation, allowed headers,
dynamic provider, then static binding headers. Each call uses its original current
agent context. Connection-owned background traffic and termination use the
provider references described in [managed shutdown](mcp-managed-shutdown.md).

An optional provider authority identity establishes equivalence across rotations
of its delegated credentials. STS derives that identity from its existing
session and subject discriminator, independently of a cache entry's custody
generation. Resolution still checks the generation, expiry and eviction; an
identity does not authorize a request or keep credentials alive. A new inbound
opaque bearer selects a different STS cache subject. Without a provider guarantee,
the host fingerprints resolved authority and cannot infer equivalence across
credential changes. Fingerprints and binding identities stay inside the host.

Calls in a relationship are serialized with cancellable admission. Different
relationships execute independently. Ending one invocation releases its command
socket and current context, while the host retains the healthy SDK relationship.
Actor shutdown stops admission and applies the existing authenticated, bounded
termination accounting to all owned connections. Dead native relationships release
their local connection and pool; their termination evidence remains available
for host shutdown accounting. Browser tabs still require their domain close tool.

## Negotiation And Loss

The official MCP SDK owns negotiated versions, protocol IDs, framing and transport
behavior. Legacy Streamable HTTP and configured SSE remain supported. Modern
2026-07-28 peers supply no protocol session ID and receive no synthetic ID or
termination DELETE. Standalone commands retain their per-command lifecycle.

Missing or ended relationships return explicit state loss. A later deliberate
operation may acquire fresh custody; it cannot recover old browser tabs. Native
ADK's automatic refresher cannot replace an established scoped relationship inside
the failed operation. A private operation identity also guards `tools/call`
dispatch across hidden native retries. The HTTP boundary removes replayable POST
bodies before the actual network transport, preventing net/http credential headers
such as `Idempotency-Key` from enabling automatic mutation replay. CLI wire capture
still inspects the body before that boundary to preserve exact schemas and numbers.

Public runner tests exercise actual native tool calls and separate Bash command
processes against local MCP HTTP peers. They cover custody across invocations,
foreign conversation/branch/isolation/authority refusal, real STS same-subject
continuity, modern sessionless negotiation, ambiguous mutation with one remote
action, and concurrent caller authority/cancellation. Existing Apps, approval,
schema/allow-list, numeric/file/stdin, SSE/TLS and standalone regressions remain
part of the affected package checks.

Implementation and the reported source checks use Codex agentic coding assistance.
