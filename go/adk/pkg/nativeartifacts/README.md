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

## Bash output and instructions (#55)

Alpha5 buffered unbounded streams. An unquoted ampersand detached curl before its
output-file option, emitting 1.28 million HTML characters; line bounds did not
contain the single line. Per-stream 8 KiB yields at most 16KiB payload plus fixed
clipping notices, leaving room above a 1600-character source excerpt. Commands
must save large bodies and inspect bounded excerpts.

AgentTemplate owns full URL quoting, placement after --, body-before-extraction,
script/style exclusion and final character bounds; inherited JESE/binary image
are unchanged. Activation requires reviewed instructions and runtime together.
Tests exercise the actual executor with fake curl, the ampersand leak, large
single-line UTF-8, stderr failure, mixed streams and unchanged small output.

## Direct ask_user service provenance

BuildResumeHITLMessage replaces incoming Parts with confirmation Parts; ADK filters
confirmation events from model contents. Adjacent text therefore cannot preserve
reply provenance. Optional AskUserResponse.transport_context is structured service
metadata separate from answers: confirmation carries it and resumed tool output
returns it to the final model request. No-context parsing/Q&A remains unchanged;
human answer strings, nested delegation, SDK/provider selection are unchanged.

Expense bridge supplies latest durable slack_source plus immutable receipt/
receipt_source when present. No receipt bytes or credentials enter this metadata.
Latest source governs clarification; original attachment source stays separate.
The real ask_user/runner pause/resume regression checks exact latest/original
provenance, unchanged human answer and no-context behavior at the final request.
A wire-only check would miss confirmation filtering.

An initial extension passed the runner but failed a fresh nonfinancial live probe:
history retained context while local-qwen reported none. Provider flattening kept
only result and discarded sibling fields. The shared OpenAI chat/Responses,
Anthropic/Ollama helper now preserves the structured envelope only for nonempty
structured transport_context; answer result remains a string, context separate.
Absent context retains existing result/content flattening. Actual OpenAI SDK/local
HTTP regression verifies serialized envelope versus earlier Q&A-only JSON and
byte-identical no-context HTTP output. Other provider tests cover unchanged cases.
The failed probe remains failed evidence; fresh prepared live qualification is
required after review/rebuild/activation.

## Direct ask_user images (#105)

Confirmation replacement/filtering also loses adjacent images. Appending a user
image before resume fails because asynchronous response rearrangement drops user
events between ask_user and its result. The runtime retains at most two ordered
JPEG/PNG Parts totaling 3 MiB outside answers/context JSON. BeforeModel inserts an
image-only native user event immediately after the real resumed result and adds
InlineData to that initial request.

Scope is application/user/session/invocation/branch; isolation comes from the
runner's recorded user event. Model callback exposes neither Session() nor
IsolationScope(). BeforeRun captures the runner's active Session; AfterRun releases
it on completion, error/interruption/cancellation. Append uses that exact snapshot
through session.Service so SQLite optimistic timestamps advance with later runner
writes. A separate Get snapshot invalidates those writes. Stable invocation event
ID prevents duplicate history across repeated model calls; later runs use retained
history. No image enters tool JSON, versions stay unchanged, ordinary noimage HITL
and expense provenance serialization are preserved; nested remote HITL unchanged.

images_test.go enters real protobuf Parts/executor, ask_user and runner pause/
resume, then actual OpenAI SDK/local HTTP. Decoded data URLs must equal ordered
PNG/JPEG originals. It checks captions/answers/context, placement after correlated
result, repeated model calls, one retained image event, terminal completion,
reopened same-LocalSessionService SQLite followup and foreign actor DB absence;
in-memory coverage remains. Initial regression had zero provider images.

First repair passed in-memory pixels but failed persistent actor termination after
recognition: a separate append advanced DB timestamp beyond the runner snapshot.
Exact native SQLite regression now fails before repair and requires completion/
reopened followup afterward. images_lifecycle_test.go exercises completion, agent
error, interruption and cancellation; invocation capture is released across both
user/session keys. The repair changes no event ordering/answer/provenance/provider
encoding, replays no failed task and introduces no stale-write retry. Fresh native
pixel-only qualification remains required; old prepared sessions remain pinned.

## Bash stream result envelope

Bash captures at most 8 KiB per stdout/stderr stream while draining excess,
preserving UTF-8 and exit status. Failure retains stdout as well as stderr;
clipping is explicit. The guard neither cancels overflow nor stores omitted
bytes, and does not bound cumulative tokens, file tools, tool inputs or MCP output.
