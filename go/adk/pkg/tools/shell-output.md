# Shell output contract

## Bash output and instructions (#55)

Alpha5 buffered unbounded streams. An unquoted ampersand detached curl before its
output-file option, emitting 1.28 million HTML characters; line bounds did not
contain the single line. Per-stream 8 KiB yields at most 16KiB payload plus fixed
clipping notices, leaving room above a 1600-character source excerpt. Commands
must save large bodies and inspect bounded excerpts.

AgentTemplate owns full URL quoting, placement after --, body-before-extraction,
script/style exclusion and final character bounds; inherited JESE/binary image
are unchanged. Activation requires reviewed instructions and runtime together.
Tests exercise the actual executor with fake curl, the ampersand leak, large
single-line UTF-8, stderr failure, mixed streams and unchanged small output.

## Bash stream result envelope

Bash captures at most 8 KiB per stdout/stderr stream while draining excess,
preserving UTF-8 and exit status. Failure retains stdout as well as stderr;
clipping is explicit. The guard neither cancels overflow nor stores omitted
bytes, and does not bound cumulative tokens, file tools, tool inputs or MCP output.

Run the adjacent output_guard_test.go and shell process tests through
`go test -race ./adk/pkg/tools` from go/.
