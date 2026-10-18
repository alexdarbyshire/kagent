# Using MCP commands in the Go runtime

An MCP binding with `exposeAsCLI: true` becomes a local executable whose name is
the referenced RemoteMCPServer name. Its tools become subcommands. The agent
discovers commands through its initial guidance and invokes them with its Bash
tool; no separately configured skill is required. This guide illustrates a
read-only inventory server. Replace the server, tool names, arguments, and result
fields with those exposed by your endpoint.

## Prepare the runtime image

This integration supports the kagent Go ADK image only. Build `go/Dockerfile`
with `BUILD_PACKAGE=adk/cmd/main.go` to package `/app` and `mcp-cli`. The base
runtime includes Bash but does not include `jq`. If your processing requires
`jq`, the operator must build a derived image that installs it, for example:

```dockerfile
FROM registry.example/kagent/go-adk@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
USER 0
RUN apk add --no-cache jq
USER 65532:65532
```

Build and publish through your normal image workflow, then pin the resulting
registry digest in the Harness. Installing a processor locally does not make it
available in the agent runtime. Derived images need no controller registration;
keep the compatible Go runtime and packaged `mcp-cli` bridge. The runtime fails
agent construction if the bridge is absent. Retain the image entrypoint or use
`command: ["/app"]`.

## Author the agent

The following example assumes an existing `default-model-config`, WorkerPool,
and usable snapshot location in the `kagent` namespace. Replace the illustrative
endpoint and image digest before applying it. Configure endpoint credentials
through your existing RemoteMCPServer Secret references; do not put secrets in
the prompt or command arguments.

```yaml
apiVersion: api.kagent.dev/v1alpha3
kind: RemoteMCPServer
metadata:
  name: inventory
  namespace: kagent
spec:
  description: Read-only inventory lookup and item summaries.
  protocol: STREAMABLE_HTTP
  url: https://mcp.example.com/mcp
---
apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata:
  name: inventory-assistant
  namespace: kagent
spec:
  template:
    modelConfig:
      name: default-model-config
    systemPrompt: |
      Use inventory --help and per-tool help to discover arguments.
      Fetch inventory once, save its MCP result envelope, and use jq locally
      to summarize it. Check command status before processing a result.
      Report failed calls and do not automatically repeat ambiguous failures.
    tools:
      - mcp:
          server:
            kind: RemoteMCPServer
            name: inventory
          tools: [list_items]
          exposeAsCLI: true
  harness:
    kagent: {}
    workload:
      image: registry.example/kagent/go-adk-jq@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    substrate:
      workerPoolRef:
        name: kagent-default
      snapshotPolicy:
        location: s3://example-snapshots/kagent/
```

Apply the resources, inspect `kubectl get agents.api.kagent.dev -n kagent`, and
wait for preparation readiness before creating a new Session through your
normal UI or Session client. Ask the agent to list available inventory and
summarize matching items. The command examples below run inside that Session's
Bash tool, where the runtime supplies the command PATH and authentication
handoff. They are not commands installed on the operator's workstation.

The same `tools` configuration works in a referenced AgentTemplate. Omitted or
false `exposeAsCLI` retains native MCP presentation. Different bindings can mix
native and CLI presentation. A nonempty `tools` list filters help and invocation;
omitted or empty lists expose all endpoint tools. Names must not collide with
other bindings or platform commands in the same agent.

## Discover and supply arguments

```sh
inventory --help
inventory list_items --help
inventory list_items --help schema
# Assuming help declares string category, integer limit, and boolean available:
inventory list_items --category office --limit 20 --available true
```

Operation help lists actual flags and their parse modes; `--help schema` prints
the full original schema. Neither invokes the operation. Property spelling
matches the MCP schema. Unique immediate children of top-level object groups
use unqualified flags; collisions use the actual group name, such as `--path.id`
and `--body.id`. Remaining literal properties retain their names.

Fully expanded wrappers are removed. For Gitea `issueCreateComment`, `--owner`,
`--repo`, `--index`, and literal `--body` assemble the original nested request.
The old whole-object `--body` syntax is intentionally retired for that schema;
existing JSON callers use `--input-file` with the original shape.

Unambiguous string flags take literal text. Other parameters parse valid JSON
with its type preserved. When strings are permitted, non-JSON text is accepted
as a string: `--value hello` is text, `--value 123` is a number, and
`--value='"123"'` is a numeric-looking string. Invalid combinations fail full
validation without reinterpreting or retrying the value. Duplicate JSON keys
remain errors. Arrays and nested objects remain single JSON values.
Branch-only fields are exposed, while
the provider's complete schema validates conditional requirements and permitted
combinations. Defaults are documented without being inserted; omitted groups
stay absent until a child is supplied.

For explicit empty objects, dynamic keys, unprojected fields, or reserved literal
properties named `help`, `input-file`, `base64-file`, `text-file`, `output-file`,
`save-text`, `save-base64`, `save-text-dir`, `save-base64-dir` or
`verify-sha256`, supply one JSON object instead:

```sh
cat > arguments.json <<'JSON'
{"category":"office","limit":20,"available":true}
JSON
inventory list_items --input-file arguments.json
cat arguments.json | inventory list_items --input-file -
```

These are alternative invocations, not a sequence to run repeatedly. Do not mix
JSON input and property flags. Reading the file sends parsed argument values,
not an upload of the file. Invalid input fails before tool dispatch.

## Supply local file values

Target the parameter names shown by operation help. For a document conversion
binding exposing `submit_file_conversion`, one file supplies `base64_string`:

```sh
document-conversion submit_file_conversion \
  --filename dummy.pdf \
  --base64-file base64_string=./dummy.pdf \
  --page_start 1 --page_end 3
```

`filename` is explicit provider metadata. The local path does not determine it.
Conversion submission returns a task identifier; this command does not wait for
completion or retrieve the result.

The same option repeats for two files. This synthetic contract illustrates a
PDF plus a DUMMY signature image; `stamp_pdf` is not an existing estate API:

```sh
pdf-fixture stamp_pdf \
  --base64-file pdf_base64=./dummy.pdf \
  --base64-file image_base64=./dummy-signature.png \
  --edits '[{"type":"image","page":1,"rect":{"x":72,"y":640,"width":150,"height":50}}]'
```

For literal UTF-8 text, use `--text-file NAME=PATH`; trailing newlines and
numeric-looking text remain strings. Both file options accept `--OPTION=NAME=PATH`
and paths containing `=`. They mix with ordinary flags, reject duplicate or
overlapping assignments, and remain exclusive of whole-request `--input-file`.
There is no nested pointer syntax or automatic encoding based on field names.

All files are read by the command in its working directory before dispatch.
Only regular files are accepted, including symlinks to regular files. Limits are
8 MiB per file, 16 MiB total raw bytes and 24 MiB assembled JSON; provider schema
constraints may be smaller. A failing second file prevents the entire call.
Local diagnostics omit file contents, while upstream results retain their normal
envelope and may echo input. Help does not read local file values.

## Save the envelope and process locally

Successful stdout contains the complete MCP `CallToolResult` JSON envelope,
including `content`, optional `structuredContent`, and metadata. A tool's
`isError: true` result also appears on stdout but exits 1. Help and successful
execution exit 0. Diagnostics go to stderr in both standalone and managed mode.

Invalid usage exits 2 before tool dispatch: unknown/excluded tools, unknown flags,
missing values, conflicting modes, duplicate assignments or JSON keys, malformed
JSON, full-schema rejection, invalid file syntax, UTF-8 or input-size violations.
Initialization and catalog requests may occur; no `tools/call` occurs.

Operational failures exit 1, including unreadable/non-regular input files,
existing output destinations, invalid private bindings or discovered schemas,
discovery, authority, transport, protocol, transformation, cancellation and
returned close/finalization errors. Empty/dash output path syntax instead exits 2.
If usage and operational failures are joined, exit 1 takes precedence and both
diagnostics remain available. Existing nonfatal remote-termination warnings keep
a successful command at exit 0. Exit 1 never authorizes a retry; the operation
may already have run, and an envelope may not be available.

Use `--output-file PATH` to save that envelope instead of emitting it on stdout:

```bash
inventory list_items --input-file arguments.json --output-file result.json
```

The command creates a new mode-0600 file relative to its working directory before
calling the tool. Existing paths, including symlinks, are refused without a tool
call. The option works with JSON input, ordinary flags and parameter-targeted file
inputs. It cannot be combined with help. Saved JSON preserves the complete result
and exact numbers, with a 32 MiB file limit including the trailing newline. This
is a local save limit, not a bound on transport buffering.

A complete error envelope is retained, and the command still exits nonzero.
Empty content arrays and text resembling help remain result envelopes. The
trusted managed result adapter runs before output reaches stdout or the file.
Missing responses and failed writes remove the incomplete file; cleanup failures
are reported. A local save failure never retries the remote operation. Saving
raw JSON does not decode a PDF/image, fetch a URL, or establish provider-specific
success.

### Save one named string

Add one `--save-text NAME=PATH` or `--save-base64 NAME=PATH` to save a returned
string without emitting its contents on stdout. Both require `--output-file`
for the complete source response. For example, when the returned object contains
`output.base64_string` and `output.sha256`:

```bash
documents convert --input-file arguments.json \
  --save-base64 output.base64_string=document.bin \
  --verify-sha256 output.sha256 --output-file response.json
```

Names come from the actual result; no output schema or pre-call discovery is
required. Top-level `structuredContent` properties are literal names. Immediate
children of object properties also have `group.child` names and unique short
names. Literal names take precedence, including names containing dots. Ambiguous
generated names fail; deeper objects and arrays are not flattened.

Native MCP content names are `content.text`, `content.image`,
`content.resource.text` and `content.resource.blob`. Shortcuts `text`, `image`,
`resource.text` and `resource.blob` work when unoccupied and unambiguous. Exactly
one matching string is required, including for native content: two image blocks
are not a request to save the first image. When `structuredContent` is present,
compatibility text blocks are suppressed but native images and resources remain
available. Non-object `structuredContent` fails selection. Without it, text is
literal even if it contains JSON; there is no embedded-JSON navigation.

Text saving preserves UTF-8 bytes and newlines, including an empty file. Base64
saving requires canonical RFC 4648 standard padded encoding without whitespace.
Selected paths reject duplicate JSON members, malformed UTF-8 and unpaired
surrogates. Unselected fields, duplicate metadata and exact numbers remain opaque
in the raw envelope. No MIME, filename, URL or document-validity inference occurs.
The optional `--verify-sha256 NAME` selects a 64-hex-digit expected digest and
compares it with the saved bytes; digest-looking fields are ignored unless chosen.

Save options accept both `--option value` and `--option=value`; split `NAME=PATH`
at the first equals sign so paths can contain equals signs. They compose with
all input modes. A command accepts only one save request and one digest option.
The source and artifact must be distinct new mode-0600 files, reserved before
dispatch, with existing parents. Existing files, symlinks, directories and FIFOs
are refused with zero tool calls. The raw file is finalized first; selection,
conversion, integrity or artifact failures retain it and remove the owned
incomplete artifact, preserving replacements at the same path. Empty artifacts
are synced and closed too. A mechanically extractable error result may be saved,
while its original operational failure still determines exit status.

The 32 MiB source limit includes the saved trailing newline. Selected strings and
text files are limited to 32 MiB; decoded binary files to 24 MiB. The source bound
also limits how much selected data can fit in an envelope. Selection and local
I/O failures exit 1 and never repeat the remote call. These limits govern local
saving, not transport buffering or provider success.

### Save a named collection

Use `--save-text-dir GROUP.FIELD=DIR` or `--save-base64-dir GROUP.FIELD=DIR`
for all rows of a top-level array of objects. Native content names also select
all matching blocks in wire order. The command still accepts exactly one save
request, requires `--output-file`, and keeps stdout empty. For a PDF render result
whose `images` rows contain `base64_string` and `sha256`:

```bash
documents render --input-file render.json \
  --save-base64-dir images.base64_string=pages \
  --verify-sha256 images.sha256 --output-file render-response.json
```

The same mechanics accept a browser-shaped `downloads.dataBase64` array when a
provider returns one. Saving does not request download bytes, fetch URLs, poll,
consume downloads, acknowledge them, or change provider state. Provider inputs
choose the returned resources; no local row filter or first-item fallback exists.

Every row must be an object with the selected string member. Missing arrays,
missing/null/non-string members and skipped or unreadable download data fail the
entire collection. An empty existing array succeeds with zero files, without
claiming its selected member was observed. Array fields require `GROUP.FIELD`;
they do not gain short aliases or recursive navigation. Literal dotted properties
still take precedence and fail collection selection rather than falling back.
Single-file flags continue to require one string and never project an array.
An explicitly selected collection digest must come from the same array group and
same row; equally sized unrelated arrays cannot be paired. Native content groups
may use a singular named digest, checked against every selected member.

The directory must be new, with an existing parent. It is reserved before dispatch
with mode 0700; existing directories, files and symlinks are refused. The raw path
must be distinct and outside it. Files are mode 0600, named `0001`, `0002`, and so
on in response order, without inferred extensions. Page numbers, IDs, suggested
filenames, MIME types and URLs remain in the complete raw envelope and never
become filesystem paths.

`manifest.json` is written and finalized after all numbered files. Its version-1
record contains `version`, the requested `target`, `conversion` (`text` or
`base64`), `source_sha256` over the exact retained raw file including its trailing
newline, and a `files` array. Each file record contains `name`, zero-based
`source_index` in the original array or native content, `length` in bytes, and
computed `sha256`. No provider payload is copied into the manifest. Empty
collections have `files: []`.

Consumers treat a directory without a complete manifest as unfinished. A failure
or cancellation removes owned collection files and the owned directory, preserves
replacements, reports cleanup failures and retains finalized raw JSON. A crash can
leave an unfinished directory; there is no cross-file transaction or automatic
crash cleanup. Mechanically saved tool-error or unsupported results still exit 1.

Collections allow at most 100 files and 32 MiB aggregate materialized payload.
The manifest is limited to 1 MiB. Existing per-file and 32 MiB raw limits still
apply, including gateway JSON-text compatibility copies. Several 20 MiB browser
downloads will not fit; exceeding a limit fails without another call or a larger
limit.

### Recover from a saved result

When live selection, decoding, digest verification or artifact writing fails,
retain the complete response and correct the local save request. Recovery uses
the packaged `mcp-cli` executable directly, even after the provider or managed
socket becomes unavailable:

```sh
mcp-cli extract --from-file response.json \
  --save-base64 output.base64_string=recovered.bin \
  --verify-sha256 output.sha256
mcp-cli extract --from-file render-response.json \
  --save-base64-dir images.base64_string=recovered-pages
mcp-cli extract --help
```

These are illustrative field names; choose fields present in the retained
envelope. Recovery accepts the same four save options, digest selection and
name/conversion/collection rules as live saving. It reads one bounded regular
file and never reads a binding, opens a managed socket, discovers tools,
transforms the saved result again or calls a provider. No additional
`--output-file`, stdin source or live input flags are accepted.

The source must contain one MCP result envelope, not a JSON-RPC wrapper, within
32 MiB including whitespace. Source bytes remain unchanged on success and
failure. The existing exclusive destination rules reject source/output aliases,
clobbering and symlink destinations. Collections hash the exact source bytes,
so extracting the same retained file produces the same manifest provenance.

Malformed syntax, JSON or envelope structure exits 2. Source I/O, size bounds,
materialization and output failures exit 1. Embedded `isError: true` or an
unsupported `resultType` also exits 1 while allowing mechanical saving. Empty
`content` is valid but offers no native output. Exit 0 reports mechanical success;
it cannot reconstruct or certify the original process's transport,
transformation or teardown status. Extraction stdout remains empty and failures
never authorize replaying the original tool call.

The optional `TestCLIPinnedGatewayProjection` test uses synthetic loopback HTTP
responses and a SHA-256-pinned agentgateway 1.6.0 binary. Set
`KAGENT_TEST_AGENTGATEWAY` to that binary and `KAGENT_TEST_MCP_CLI` to the candidate
executable; the test requires Python 3 and otherwise reports an explicit skip
when binary paths are unset. It verifies JSON/base64 and native PNG recovery and
characterizes raw PDF as lossy text. Saving cannot restore bytes lost upstream.

The Go Bash tool returns an execution error when a shell command exits nonzero,
so do not rely on it displaying that command's captured stdout. Save the
envelope, handle failure inside the script, and print the saved envelope
when needed. For example, run this as one Bash invocation:

```bash
if inventory list_items --input-file arguments.json --output-file result.json 2> diagnostic.txt; then
  # This example assumes the endpoint returns structuredContent.items.
  jq -e '.structuredContent.items | type == "array"' result.json >/dev/null || {
    printf '%s\n' 'Expected structuredContent.items array; inspect result.json.'
    cat result.json
    exit 0
  }
  jq '{count: (.structuredContent.items | length),
       names: [.structuredContent.items[] | .name]}' result.json
else
  status=$?
  printf 'MCP command failed (exit %s).\n' "$status"
  cat diagnostic.txt
  if [ -s result.json ]; then
    cat result.json
  fi
fi
```

The shell handles failure so the Bash tool can display the diagnostics; this
does not turn a failed MCP call into a successful result. Do not process an
error envelope as inventory. `structuredContent` is optional: inspect the
actual envelope before choosing a selector. If the server instead returns JSON
inside a text content block, select and parse that documented block explicitly;
do not assume `.content[0].text` is always JSON. Keep the full envelope in the
Session working directory while presenting only the useful summary to the
model. This is a processing pattern, not evidence of token savings.

## Lifecycle and limits

The adapter produces private typed catalog, operation and schema help, result,
or failure outcomes. Semantic help construction resolves schema annotations and
parameter metadata; the renderer only formats that data. Results carry their
captured envelope and any operational failure together. Managed handoff retains
those distinctions and rejects unknown, inconsistent or request-mismatched
outcomes before rendering or saving. Its checks apply to adapter-owned fields;
provider result fields remain under the existing MCP protocol semantics.

The command subprocess parses option syntax once, reads local input files and
reserves raw and artifact destinations. Its typed invocation carries assignments or
whole-request bytes across the private socket; local paths and reconstructed
argv do not cross that boundary. It also owns output finalization and combines
execution diagnostics with local write/cleanup errors. Standalone and managed
execution deliver their typed outcome to the same command-local handler, which
finalizes raw output before named materialization. There is no stdout reparsing
or second result request. Pure result projection owns names, source identities,
selected-string conversion and same-row digest checking; the local writer owns
exclusive reservation, sync/close, collection manifests and cleanup. Collection
children use the reserved directory handle for writes and cleanup, so replacing
its public path cannot redirect them into a different directory.
Local `extract` branches before binding and socket handling. Its bounded source
reader and envelope validation feed the same materializer and local writer,
without a client or a second transformation. Protocol status fields are checked
while unselected provider values remain opaque; selected paths retain the shared
strict string, duplicate-member, conversion and integrity checks.

Each selected tool gets one private, immutable command plan from that command's
live discovery. The plan owns tool identity, original schema bytes, the compiled
validator, projected flags and parse modes, semantic help, input assembly and
full-schema validation. Catalog help does not compile every tool. Plans are
neither retained between commands nor serialized over the socket, so subsequent
help and invocation observe changed provider schemas within the same connection.
The existing executor owns discovery, authority, transport and dispatch; the
renderer formats outcomes without performing those operations.

Changing binding names, endpoints, selection, descriptions, or presentation
prepares a new revision for new Sessions. Existing Sessions retain their pinned
compiled bindings. Suspend/resume restores commands from those inputs and the
pinned image; remote catalogs and schemas remain live and are not frozen by
revision pinning.

Commands invoked through native Bash share an actor-owned MCP connection for
the same application, user, Session, agent, branch and isolation scope. Separate
command processes and follow-up invocations therefore retain browser ownership.
The invocation socket supplies the current caller context; a command cannot
choose a scope, endpoint or credentials. Each remote request resolves current
invocation headers. Different callers and agent scopes use separate connections.
Actor shutdown closes retained connections. An expired or lost remote session
is an explicit error; commands do not replay actions or recover old browser tabs.
Actor replacement or suspend/resume starts fresh remote sessions.

`requireApproval: true` together with CLI exposure fails preparation, including
Shared children. Keep protected bindings native. Command visibility and tool
selection are presentation controls; endpoint/gateway authorization, Substrate
credential injection, egress policy, and existing runtime controls still apply.
Python kagent, Codex, Claude, and BYO are unsupported for this CLI integration.
Unsupported Harnesses and entrypoint overrides fail preparation.

Streamable HTTP and SSE are supported with their configured routes. The bridge
does not follow redirects or automatically replay failed tool calls.
Cancellation closes local work but does not roll back an upstream operation;
check its outcome before deciding whether to retry. MCP resources, prompts,
and Apps are not converted to commands. Sampling, elicitation, roots, and
input-required continuations are unsupported. External JSON schema references
and unsupported transport settings fail explicitly. See
[configuration and compilation](configuration-and-compilation.md#mcp-commands-in-the-go-runtime)
and the [bridge contract](../../go/adk/cmd/mcp-cli/README.md) for detailed limits.
