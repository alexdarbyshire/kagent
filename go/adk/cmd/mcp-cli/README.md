# Standalone MCP CLI bridge

Build from `go/`:

```sh
go build -o mcp-cli ./adk/cmd/mcp-cli
```

The bridge consumes a private binding file supplied by its launcher. It is not
an endpoint configuration interface for the model. This standalone executable
currently supports Linux with procfs and Streamable HTTP. Agent YAML, command
materialization, and runtime image packaging are not integrated yet.

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
settings are supported. Credential injection and invocation-scoped identity will
be integrated through the existing runtime path. Keep credentials out of compiled
binding artifacts
and model guidance. Command identity and connection settings come from this file;
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

SIGINT, SIGTERM, and the binding timeout cancel local work and close connection
resources. Teardown is bounded even when the server does not answer its session
termination request. Cancellation does not imply rollback of an upstream
operation; an interrupted call can have an ambiguous outcome.

This bridge rejects `require_approval: true`; retain native MCP presentation
for protected bindings. Invocation header forwarding (`allowed_headers`), SSE
transport/read timeout, and disabling session termination are unsupported here
and fail explicitly. External JSON schema references are unsupported. Tool
schemas and result envelopes retain exact wire precision within the SDK's
float64 numeric range; literals outside that range (for example, `1e400`) fail
SDK decoding with an explicit error. Argument numbers retain their original JSON
precision. The client does not advertise sampling, elicitation, or roots
capabilities; input continuations are reported as unsupported rather than
automatically replayed.
MCP resources, prompts, and Apps are not converted to commands.
