# Standalone MCP CLI bridge

Build from `go/`:

```sh
go build -o mcp-cli ./adk/cmd/mcp-cli
```

For live calls, the bridge consumes a private binding file supplied by its launcher. It is not
an endpoint configuration interface for the model. This standalone executable
supports Linux with procfs, Streamable HTTP, and SSE. The Go ADK image packages
it and materializes per-agent commands from compiled Agent YAML. For public
configuration and a processing example, see the
[MCP command usage guide](../../../../docs/architecture/mcp-command-usage.md).

## Recover a saved result locally

The same executable can extract named strings from a saved MCP result without
a binding, credentials, discovery or a running provider. This synthetic example
creates a two-byte file containing `00 ff`, with empty stdout:

```sh
printf '%s\n' '{"content":[],"structuredContent":{"output":{"base64_string":"AP8="}}}' > sample-result.json
./mcp-cli extract --from-file sample-result.json \
  --save-base64 output.base64_string=sample.bin
./mcp-cli extract --help
```

Use this mode after a live `--save-*` operation fails locally but leaves its
complete `--output-file` response. Correct the target, digest selection or output
path and extract from that file; the tool is never called again. `--save-text`
preserves a string's UTF-8 bytes. `--save-text-dir` and `--save-base64-dir` save
collections with the same ordinal files and manifest as live saving. Optional
`--verify-sha256 NAME` checks a digest selected from the same result. See
[named output and recovery rules](../../../../docs/architecture/mcp-command-usage.md#recover-from-a-saved-result)
for naming, bounds and status semantics.

The source must be one regular file containing an MCP result envelope, at most
32 MiB. Source bytes remain unchanged; collection manifests hash those exact
bytes, including any whitespace. Destinations must be new files or directories;
existing paths and symlinks are refused. Recovery accepts no `--output-file`,
stdin source, binding or live input flags and never transforms the result again.
Malformed syntax/envelopes exit 2; local I/O, materialization and embedded MCP
failures exit 1. Mechanically extractable `isError` or unsupported `resultType`
envelopes can still produce artifacts while exiting 1. Exit 0 cannot reconstruct
the original process's transport, transformation or teardown status.

## Invoke a live tool

The command surface draws pattern inspiration from
[MCPorter](https://github.com/openclaw/mcporter), the Python
[mcp2cli](https://github.com/knowsuchagency/mcp2cli) and Rust
[mcp2cli](https://github.com/mcp2cli/source-code), and
[mcpc](https://github.com/apify/mcpc). This fork does not embed or depend on
those implementations. Their command projection, exact text-file handling,
complete-result preservation, and resource selection/materialization patterns
informed this adapter's kagent-owned command plan and local file behavior.

```json
{
  "name": "browser",
  "http": {
    "params": {
      "url": "https://mcp.example.test/mcp",
      "headers": {"X-Example": "demo"},
      "timeout": 30
    },
    "tools": ["navigate"],
    "require_approval": false
  }
}
```

The `http` object uses the existing Go MCP configuration fields. Omitted or
empty `tools` selects all available tools. Static headers and existing TLS
settings are supported. The integrated runtime preserves Substrate credential
injection and provides invocation-scoped identity through a private handoff.
Keep credentials out of compiled binding artifacts and model guidance.
Command identity and connection settings come from this file;
there are no tool-command endpoint flags.

```sh
./mcp-cli --binding-file binding.json --help
./mcp-cli --binding-file binding.json navigate --help
./mcp-cli --binding-file binding.json navigate --help schema
./mcp-cli --binding-file binding.json navigate --url 'https://example.test'
./mcp-cli --binding-file binding.json navigate --url=https://example.test
./mcp-cli --binding-file binding.json navigate --input-file arguments.json
printf '%s\n' '{"url":"https://example.test"}' |
  ./mcp-cli --binding-file binding.json navigate --input-file -
```

Operation help lists available flags, parse modes, descriptions and requirements.
`TOOL --help schema` prints the original schema with exact numeric values. Both
help modes discover tools without invoking an operation.

Arguments preserve MCP property spelling. Immediate children of top-level object
groups become unqualified flags when unique; collisions use the discovered group
and a dot, such as `--path.id` and `--body.id`. Remaining literal properties win
collisions. Fully expanded group wrappers disappear: for Gitea
`issueCreateComment`, `--owner alice --repo demo --index 12 --body 'Comment text'`
assembles `path.owner`, `path.repo`, `path.index`, and `body.body`. This intentionally
retires the old whole-object `--body` spelling for that schema. Use whole-request
JSON in its original shape for existing callers that supply a complete body.

Unambiguously string-valued parameters take literal text. Other parameters parse
valid JSON with its original type. If a parameter permits strings, non-JSON text
is accepted as a string: `--value hello` supplies text and `--value 123` supplies
a number. Use `--value='"123"'` for a numeric-looking string. Failed schema
validation never reinterprets a JSON value as text. Syntactically valid JSON with
duplicate keys remains an error. Malformed structured-looking input is text only
when strings are permitted; otherwise it is invalid JSON.

Arrays and nested objects remain JSON values; arrays have no indexed flags.
Local references and
composition/conditional branches contribute declared names; the original full
schema validates the assembled request and all conditional combinations.
Defaults in help are documentation only. Omitted flags remain absent, and a group
is created only when a child is supplied.

Help and invocation use one command plan compiled from live tool discovery.
Each command refreshes that contract even when managed commands share a retained
connection. The subprocess parses options once and resolves local files; the
actor receives typed values without local paths or reconstructed command lines.
The plan owns projection, help data, assembly and validation. The existing
executor owns discovery and the tool call; the renderer only formats outcomes.

Use `--input-file` for an explicit empty object, dynamic keys, unprojected fields,
or reserved literal `help`, `input-file`, `base64-file`, and `text-file` properties. Groups that cannot be
fully projected retain a JSON-valued wrapper. JSON input must be one object and
cannot be combined with property flags. No arguments supplies an empty object.
Duplicate keys at any depth, repeated or overlapping parent/child assignments,
unknown flags, and schema violations fail before tool dispatch. JSON properties
follow the full schema, including its additional-property rules.
Reading an input file sends its parsed arguments, not the file itself. Binding
and argument paths must be regular files; stdin is available for pipe input.

Use repeatable `--base64-file NAME=PATH` or `--text-file NAME=PATH` to supply a
string from a local file to an exposed parameter name. The `--base64-file=NAME=PATH`
and `--text-file=NAME=PATH` spellings also work. The path may contain `=`.
Base64 encoding is standard and padded; text must be UTF-8 and retains newlines.
Text is never interpreted as JSON, even for mixed-type parameters. These options
mix with ordinary flags but cannot overwrite another assignment or accompany
`--input-file`. There are no nested target paths, filename inference or stdin
file values. Help lists target names without reading files.

File inputs accept regular files, including symlinks to regular files, with
limits of 8 MiB per file, 16 MiB aggregate raw bytes and 24 MiB assembled JSON.
Provider schemas may impose lower limits. The command process reads files in
its own working directory, including under managed execution; values cross the
private invocation socket, never OS command arguments. A local read, size or
validation failure dispatches no tool call and does not print file contents.
An upstream tool can still echo input in its result envelope.

Successful stdout is the MCP `CallToolResult` JSON envelope, preserving content,
structured content, and metadata. An `isError` result also appears on stdout,
with a diagnostic on stderr and exit status 1. Help uses stdout. Exit status is
the same in standalone and managed commands:

| Exit | Meaning |
| --- | --- |
| 0 | Successful help or execution |
| 2 | Invalid usage rejected before tool dispatch: unknown tools/flags, malformed arguments, schema violations, invalid file syntax, UTF-8 or input-size violations |
| 1 | Operational failure: local I/O, binding/discovery/schema errors, authority, transport, protocol, tool errors, transformation, cancellation or returned finalization errors |

An unreadable input file is an operational failure even though no tool was
called. Empty or dash output paths are usage errors; an existing destination is
an operational failure. Joined operational and usage failures exit 1 and retain
both diagnostics. Nonfatal remote-termination warnings keep a successful command
at exit 0. Exit 1 never authorizes a retry: execution may already have occurred.
HTTP redirects and transport replay are disabled for MCP POSTs.

Use `--output-file PATH` to save the complete raw envelope instead of stdout.
The command exclusively creates a mode-0600 file and retains complete error
envelopes even at exit 1. Empty content arrays are complete responses. Missing
responses and failed writes remove the owned incomplete file, with cleanup
errors reported. The 32 MiB limit includes the trailing newline. The trusted
managed result adapter runs before rendering or saving; saving makes no claim
about provider-specific document validity. See the usage guide for processing
results from Bash, which reports stderr when commands fail.

Standalone SIGINT, SIGTERM, and the binding timeout cancel local work and close
connection resources. Inside native Bash, the command delegates to its actor's
private invocation socket. The actor retains a connection scoped to the actual
application/user/Session/agent/branch/isolation scope across commands and resolves
current invocation headers on each request. The command cannot choose its scope
or endpoint. Cancellation ends the call; actor shutdown closes retained sessions.
An expired or lost session is an explicit error, without action replay or recovery
of old browser ownership. Input files are read relative to the command's working
directory before delegation; JSON values and raw result numbers retain precision.
Teardown is bounded even when the server does not answer its session
termination request. Cancellation does not imply rollback of an upstream
operation; an interrupted call can have an ambiguous outcome.

This bridge rejects `require_approval: true`; retain native MCP presentation
for protected bindings. Commands requiring invocation headers fail explicitly
without the runtime handoff. SSE supports its native read timeout; Streamable HTTP
`sse_read_timeout` and disabling session termination are unsupported and fail
explicitly. External JSON schema references are unsupported. Tool
schemas and result envelopes retain exact wire precision within the SDK's
float64 numeric range; literals outside that range (for example, `1e400`) fail
SDK decoding with an explicit error. Argument numbers retain their original JSON
precision. The client does not advertise sampling, elicitation, or roots
capabilities; input continuations are reported as unsupported rather than
automatically replayed.
MCP resources, prompts, and Apps are not converted to commands.
