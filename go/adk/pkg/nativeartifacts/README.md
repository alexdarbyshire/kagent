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

## Managed transport ownership

The existing MCP runtime owns client lifetime, reconnect policy and standalone
SSE behavior. The artifact adapter transforms invocation results on that same
managed relationship; it creates no production transport. Historical standalone
HTTP discovery wrappers and their recipe-only tests are retired. Existing managed
MCP regressions and cli_artifacts_test.go cover the current public boundary.
No action, publication or failed tool call is replayed by image adaptation.

## Related execution contracts

[Shell output](../tools/shell-output.md) owns stream limits and failure semantics.
[Human input](../a2a/human-input.md) owns resume images and service provenance.

Run `go test -race ./adk/pkg/nativeartifacts ./adk/pkg/mcp` from go/.
