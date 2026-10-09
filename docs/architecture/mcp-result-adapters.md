# MCP result adapters

A host may install `WithResultTransformer` before constructing its agent. The
callback receives the current trusted invocation and the result JSON. It belongs
to active operations, not to a cached invocation. The caller must preserve
non-image fields and decide artifact lifetime through its existing service.

Integrated commands transform the captured raw MCP result after the SDK call
and before stdout reaches the subprocess. Unadapted commands retain their raw
wire result, including JSON numbers. Adapter failures are explicit and never
replay a remote operation. Standalone commands do not acquire the actor's adapter.

Native tools install the same callback on the client already connected through
the shared managed transport. Google ADK retains tool conversion and confirmation;
managed lifecycle ownership, scope selection and mutation guards are unchanged.
When no callback is installed, ADK constructs its usual default client.

The estate's screenshot adapter is maintained separately from this runtime.
It uses the original native artifact context and actor memory service; no artifact
storage, MIME rules, decoding policy or retention framework is defined here.
