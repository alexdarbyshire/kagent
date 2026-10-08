# Standalone MCP CLI bridge

Build from `go/`:

```sh
go build -o mcp-cli ./adk/cmd/mcp-cli
```

The bridge consumes a private binding file supplied by its launcher. It is not
an endpoint configuration interface for the model. This standalone executable
supports Linux with procfs, Streamable HTTP, and SSE. The Go ADK image packages
it and materializes per-agent commands from compiled Agent YAML. For public
configuration and a processing example, see the
[MCP command usage guide](../../../../docs/architecture/mcp-command-usage.md).

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
./mcp-cli --binding-file binding.json navigate --url 'https://example.test'
./mcp-cli --binding-file binding.json navigate --url=https://example.test
./mcp-cli --binding-file binding.json navigate --input-file arguments.json
printf '%s\n' '{"url":"https://example.test"}' |
  ./mcp-cli --binding-file binding.json navigate --input-file -
```

Arguments use original MCP property names. Strings are literal; numbers,
integers, booleans, arrays, and objects use schema-typed JSON values. Use JSON
input for complex or ambiguous schemas, explicit null, and properties named
`help` or `input-file`. JSON input must be one object and cannot be combined
with property flags. No arguments supplies an empty object. Duplicate keys or
flags, unknown flags, and schema violations fail before tool dispatch. JSON
properties follow the full schema, including its additional-property rules.
Reading an input file sends its parsed arguments, not the file itself. Binding
and argument paths must be regular files; stdin is available for pipe input.

Successful stdout is the MCP `CallToolResult` JSON envelope, preserving content,
structured content, and metadata. An `isError` result also appears on stdout,
with a diagnostic on stderr and exit status 1. Input, protocol, connection, and
unsupported-interaction errors use stderr and exit status 1. Help uses stdout.
No failure automatically retries the tool call. HTTP redirects and transport
replay are disabled for MCP POSTs.

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
