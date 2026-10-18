# Gateway schema fixtures

These input schemas were emitted by agentgateway 1.6.0, revision `ea560864`,
from the estate crawl, conversion, Gitea issues, and PDF OpenAPI contracts on
2026-10-17. They are discovery results, indented as JSON without changing values.
The gateway binary SHA-256 is
`da103875206c49ebb0f0b732ff201a33c4e11c55126fdb6b5abf970e89e2908c`.

Executable CLI tests serve these schemas from a local recording MCP server and
assert the original nested arguments. They perform no provider operation. The
PDF fixture receives synthetic input and a literal DUMMY/date edit; it does not
exercise PDF processing. Synthetic schemas in the test cover collisions and
composition cases absent from these gateway outputs.
