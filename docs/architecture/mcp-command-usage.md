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
properties named `help`, `input-file`, `base64-file` or `text-file`, supply one JSON object instead:

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
`isError: true` result also appears on stdout but exits nonzero. Input,
connection, protocol, and unsupported-interaction errors report diagnostics on
stderr and exit nonzero; an envelope may not be available.

The Go Bash tool returns an execution error when a shell command exits nonzero,
so do not rely on it displaying that command's captured stdout. Redirect the
envelope first, handle failure inside the script, and print the saved envelope
when needed. For example, run this as one Bash invocation:

```bash
if inventory list_items --input-file arguments.json > result.json 2> diagnostic.txt; then
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
