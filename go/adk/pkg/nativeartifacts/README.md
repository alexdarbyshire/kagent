# Native result and continuation contract

This package owns image adaptation and its process-memory lifetime. The adjacent
Go regressions exercise original pixels, full batch validation, session isolation
and process loss. The runner installs it before agent construction; managed MCP
client ownership remains with the existing runtime. Estate composition selects
immutable fork revisions and owns role packaging, image publication and deployment.

## Screenshot storage and model delivery

Sending middleware intercepts ImageContent on the ADK toolset's existing MCP
session. A thin wrapper binds invocation-local artifact access through
Context.Value because ADK 2.4.0 lacks WithAgentContext. Save scopes a fresh UUID
filename to the invoking application/user/session, without a user: prefix.
The envelope retains structured/nonimage content plus references. Native
load_artifacts puts original bytes in model Parts, not retained session events.

Full image.Decode validation rejects truncated/corrupt payloads accepted by
DecodeConfig; the decoded raster is discarded and original bytes are preserved.
Every member validates before the first Save. Highly compressed images and
progressive JPEG coefficient planes can exceed the 3 MiB original-byte bound in
raster allocation/CPU. Adding pixel/dimension/count/raster limits would reject
currently valid input and needs explicit alignment.

## Historical transport decisions

Model latency exposed SDK 1.8.0 terminal idle-GET failure:
`standalone SSE stream: exceeded 5 retries without progress`. Repeated closures
ended the entire client; ADK reconnection changed stateful browser identity
without requiring a pod restart. DisableStandaloneSSE retains stateful headers,
JSON/SSE POST responses and legacy SSE transports, but withdraws unsolicited
server messages/requests over standalone GET. Integrations needing those messages
must revisit that policy. Actual SDK/HTTP-peer regression with 1 ms retry interval
reproduces failure before correction and same-identity calls after the idle
window afterward, in both POST-response modes without standalone GETs.

Gateway 1.6.0 reaped inactive UUID sessions after 30 min and returned HTTP 400/plain
`mcp: invalid session ID header` instead of required 404. Historical HTTP transport
normalizes only that exact response to tools/list POST at its configured endpoint
with UUID session header. Supplied client/auth/body are retained. SDK
ErrSessionMissing lets ADK's bounded discovery initialize one fresh session and
restart pagination; other statuses/bodies/methods/nonUUID sessions are unchanged.
No tools/call, publication or browser action is replayed. Inspect uncertain effects.
Fresh MCP identity creates fresh browser context: old tabs/ownership are not
recovered. Native conversations and actor-memory artifacts retain their lifetimes.

Real ADK/SDK boundary tests prove one discovery replacement and unchanged mutation
failure. Optional TestLocalGatewayIdleDiscovery uses isolated pinned gateway/
official adapter with config.mcp.sessionTtl: 1s and
NATIVE_MCP_EXPIRY_ENDPOINT. It waits 32 s for the 30 s reaper, then rediscovers on the
same toolset. Never target an estate gateway or use live credentials. The fixture
observed actual 400 then repaired discovery; production TTL is unchanged.

## Related execution contracts

[Shell output](../tools/shell-output.md) owns stream limits and failure semantics.
[Human input](../a2a/human-input.md) owns resume images and service provenance.

Run `go test -race ./adk/pkg/nativeartifacts ./adk/pkg/mcp` from go/.
