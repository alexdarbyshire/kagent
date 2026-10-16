# Human-input images and service provenance

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


Run `go test -race ./adk/pkg/a2a ./adk/pkg/models ./adk/pkg/tools` from go/.
These regressions cover runtime behavior; new image deployment and prepared
session qualification remain the estate operator's responsibility.
