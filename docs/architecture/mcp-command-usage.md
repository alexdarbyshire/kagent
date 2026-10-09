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

Build and publish through your normal image workflow. Obtain the resulting
registry digest, then register that exact derived image reference with the
controller. Installing a processor locally does not make it available in the
agent runtime. Image registration is an operator assertion that the image
contains the compatible Go runtime and bridge; it is not automatic detection.

```yaml
# Helm values excerpt; use the actual built image digest.
controller:
  env:
    - name: KAGENT_MCP_CLI_GO_IMAGES
      value: "registry.example/kagent/go-adk-jq@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
```

The controller setting is a comma-separated list and defaults to empty. Use the
identical reference in the Harness below. Registering the base image does not
register a derived image, and registering a tag does not satisfy the digest
requirement. Retain the image entrypoint or use `command: ["/app"]`.

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
# Assuming help declares string category, integer limit, and boolean available:
inventory list_items --category office --limit 20 --available true
```

Property names match the MCP schema exactly. String flags take literal strings;
numbers, booleans, arrays, and objects take schema-typed JSON values. For nested
values, explicit null, ambiguous schemas, or properties named `help` or
`input-file`, supply one JSON object instead:

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
Python kagent, Codex, Claude, BYO, and unregistered images are unsupported for
this CLI integration and fail preparation.

Streamable HTTP and SSE are supported with their configured routes. The bridge
does not follow redirects or automatically replay failed tool calls.
Cancellation closes local work but does not roll back an upstream operation;
check its outcome before deciding whether to retry. MCP resources, prompts,
and Apps are not converted to commands. Sampling, elicitation, roots, and
input-required continuations are unsupported. External JSON schema references
and unsupported transport settings fail explicitly. See
[configuration and compilation](configuration-and-compilation.md#mcp-commands-in-the-go-runtime)
and the [bridge contract](../../go/adk/cmd/mcp-cli/README.md) for detailed limits.
