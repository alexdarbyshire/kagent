package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestCLIProcessGroupedParameters(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	// The gateway's Gitea issueCreateComment groups repository identity under
	// path and comment text under body.body; the recording server never writes.
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"index":{"type":"integer"}},"required":["owner","repo","index"]},"body":{"type":"object","properties":{"body":{"type":"string","description":"Comment text"}},"required":["body"]}},"required":["path","body"],"additionalProperties":false}`)
	var calls atomic.Int32
	var received json.RawMessage
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "projection", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "comment", InputSchema: schema}, func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		received = bytes.Clone(request.Params.Arguments)
		calls.Add(1)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}, nil
	})
	remote := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer remote.Close()
	binding := filepath.Join(t.TempDir(), "binding.json")
	require.NoError(t, os.WriteFile(binding, []byte(`{"name":"fixture","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
	cmd := exec.Command(binary, "--binding-file", binding, "comment", "--owner=alice", "--repo=demo", "--index=9007199254740993", "--body=literal comment")
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Equal(t, int32(1), calls.Load())
	require.JSONEq(t, `{"path":{"owner":"alice","repo":"demo","index":9007199254740993},"body":{"body":"literal comment"}}`, string(received))
}

func TestCLIProcessParameterDiscovery(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mcp-cli")
	output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp-cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	conditional := `{"type":"object","properties":{"body":{"type":"object","properties":{"mode":{"type":"string"}},"required":["mode"],"if":{"properties":{"mode":{"const":"text"}}},"then":{"properties":{"value":{"type":"string"},"detail":{"type":"string"}},"required":["value","detail"],"not":{"required":["count"]}},"else":{"properties":{"value":{"type":"integer"},"count":{"type":"integer"}},"required":["value","count"],"not":{"required":["detail"]}}}},"required":["body"]}`
	predicate := `{"type":"object","properties":{"value":{"type":["string","integer"]},"extra":{"type":"boolean"}},"if":{"properties":{"value":{"type":"string"}}},"then":{"required":["extra"]}}`
	for _, tc := range []struct {
		name, schema, input, want string
		fixture                   string
		helpContains              []string
		helpAbsent                []string
		helpRepeats               int
		args                      []string
		invalid                   bool
	}{
		{name: "chained wrapper retirement releases names", schema: `{"type":"object","properties":{"A":{"type":"object","properties":{"x":{"type":"string"}}},"B":{"type":"object","properties":{"A.x":{"type":"string"}}},"C":{"type":"object","properties":{"x":{"type":"string"}}},"A.x":{"type":"object","properties":{"z":{"type":"string"}}},"D":{"type":"object","properties":{"B":{"type":"string"}}},"z":{"type":"string"},"A.x.z":{"type":"string"}}}`, args: []string{`--A={"x":"first"}`, "--B.A.x=second", "--C.x=third", `--A.x={"z":"fourth"}`, "--B=released", "--z=literal", "--A.x.z=dotted"}, want: `{"A":{"x":"first"},"B":{"A.x":"second"},"C":{"x":"third"},"A.x":{"z":"fourth"},"D":{"B":"released"},"z":"literal","A.x.z":"dotted"}`, helpContains: []string{"--B <literal text>"}, helpAbsent: []string{"--B <JSON>", "--D.B", "\n  --D <"}, helpRepeats: 64},
		{name: "cyclic wrapper naming uses JSON fallback", schema: `{"type":"object","properties":{"A":{"type":"object","properties":{"x":{"type":"string"}}},"C":{"type":"object","properties":{"x":{"type":"string"}}},"A.x":{"type":"object","properties":{"A.x":{"type":"string"}}}}}`, args: []string{`--A={"x":"first"}`, "--C.x=second", `--A.x={"A.x":"third"}`}, want: `{"A":{"x":"first"},"C":{"x":"second"},"A.x":{"A.x":"third"}}`, helpAbsent: []string{"--A.x.A.x", "\n  --C <"}, helpRepeats: 16},
		{name: "collision restoration is deterministic", schema: `{"type":"object","properties":{"x":{"type":"string"},"a.x":{"type":"string"},"a":{"type":"object","properties":{"x":{"type":"integer"}}},"b":{"type":"object","properties":{"a":{"type":"string"}}}}}`, args: []string{"--b.a=text", `--a={"x":1}`, "--x=literal", "--a.x=dotted"}, want: `{"b":{"a":"text"},"a":{"x":1},"x":"literal","a.x":"dotted"}`, helpAbsent: []string{"\n  --b <"}, helpRepeats: 64},
		{name: "predicate cannot narrow mixed value", schema: predicate, args: []string{"--value=123"}, want: `{"value":123}`, helpContains: []string{"--value <JSON or literal text>"}},
		{name: "predicate preserves numeric wire type", schema: predicate, args: []string{"--value=123", "--extra=true"}, want: `{"value":123,"extra":true}`},
		{name: "then cannot narrow mixed value", schema: `{"type":"object","properties":{"value":{"type":["string","integer"]},"extra":{"type":"boolean"}},"if":{"properties":{"extra":{"const":true}}},"then":{"properties":{"value":{"type":"string"}}}}`, args: []string{"--value=123", "--extra=false"}, want: `{"value":123,"extra":false}`},
		{name: "else cannot narrow mixed value", schema: `{"type":"object","properties":{"value":{"type":["string","integer"]},"extra":{"type":"boolean"}},"if":{"properties":{"extra":{"const":true}}},"else":{"properties":{"value":{"type":"string"}}}}`, args: []string{"--value=123", "--extra=true"}, want: `{"value":123,"extra":true}`},
		{name: "allOf conditional cannot narrow sibling", schema: `{"type":"object","allOf":[{"properties":{"value":{"type":["string","integer"]},"extra":{"type":"boolean"}}},{"if":{"properties":{"extra":{"const":true}}},"then":{"properties":{"value":{"type":"string"}}}}]}`, args: []string{"--value=123", "--extra=false"}, want: `{"value":123,"extra":false}`},
		{name: "conditional alternative preserves number", schema: `{"type":"object","properties":{"extra":{"type":"boolean"}},"anyOf":[{"properties":{"value":{"type":"string"}}},{"if":{"properties":{"extra":{"const":true}}},"then":{"properties":{"value":{"type":"integer"}}}}]}`, args: []string{"--value=123", "--extra=true"}, want: `{"value":123,"extra":true}`},
		{name: "branch-only string remains literal", schema: `{"type":"object","properties":{"extra":{"type":"boolean"}},"if":{"properties":{"extra":{"const":true}}},"then":{"properties":{"value":{"type":"string"}}}}`, args: []string{"--value=123", "--extra=true"}, want: `{"value":"123","extra":true}`},
		{name: "local refs", schema: `{"type":"object","properties":{"body":{"$ref":"#/$defs/body"}},"$defs":{"body":{"type":"object","properties":{"text":{"$ref":"#/$defs/text"}}},"text":{"type":"string"}}}`, args: []string{"--text=literal"}, want: `{"body":{"text":"literal"}}`},
		{name: "conditional group constraints", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"mode":{"type":"string"}}}},"if":{"properties":{"body":{"properties":{"mode":{"const":"text"}}}}},"then":{"properties":{"body":{"properties":{"text":{"type":"string"}},"required":["text"]}}}}`, args: []string{"--mode=text", "--text=hello"}, want: `{"body":{"mode":"text","text":"hello"}}`},
		{name: "conjunctive string constraints", schema: `{"type":"object","properties":{"body":{"type":"object","allOf":[{"properties":{"text":{"type":"string"}}},{"properties":{"text":{"minLength":1}}}]}}}`, args: []string{"--text=hello"}, want: `{"body":{"text":"hello"}}`},
		{name: "string enum", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"choice":{"enum":["a","b"]}}}}}`, args: []string{"--choice=a"}, want: `{"body":{"choice":"a"}}`},
		{name: "alternative unknown type stays JSON", schema: `{"type":"object","properties":{"body":{"type":"object","anyOf":[{"properties":{"text":{"type":"string"}}},{"properties":{"text":{"minLength":1}}}]}}}`, args: []string{"--text=4"}, want: `{"body":{"text":4}}`},
		{name: "actual group collisions", schema: `{"type":"object","properties":{"path":{"type":"object","properties":{"id":{"type":"integer"}}},"body":{"type":"object","properties":{"id":{"type":"string"}}}}}`, args: []string{"--path.id=12", "--body.id=literal"}, want: `{"path":{"id":12},"body":{"id":"literal"}}`},
		{name: "reserved children qualify", schema: `{"type":"object","properties":{"query":{"type":"object","properties":{"help":{"type":"string"},"input-file":{"type":"string"},"snake_case":{"type":"string"}}}}}`, args: []string{"--query.help=literal", "--query.input-file=literal", "--snake_case=preserved"}, want: `{"query":{"help":"literal","input-file":"literal","snake_case":"preserved"}}`},
		{name: "mixed group retains wrapper", schema: `{"type":"object","properties":{"body":{"type":["object","null"],"properties":{"text":{"type":"string"}}}}}`, args: []string{"--body=null"}, want: `{"body":null}`},
		{name: "empty group through JSON", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"text":{"type":"string"}}}}}`, args: []string{"--input-file=-"}, input: `{"body":{}}`, want: `{"body":{}}`},
		{name: "nullable child", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"text":{"type":["string","null"]}}}}}`, args: []string{"--text=null"}, want: `{"body":{"text":null}}`},
		{name: "duplicate child", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"text":{"type":"string"}}}}}`, args: []string{"--text=a", "--text=b"}, invalid: true},
		{name: "nested JSON duplicate", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"items":{"type":"array"}}}}}`, args: []string{`--items=[{"a":1,"a":2}]`}, invalid: true},
		{name: "exact documented default", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"count":{"type":"integer","default":9007199254740993,"description":"Exact count"}}}}}`, want: `{}`, helpContains: []string{"Default (documentation only): 9007199254740993", "Exact count", "--count <JSON>"}},
		{name: "literal dotted wins", schema: `{"type":"object","properties":{"path.id":{"type":"string"},"id":{"type":"string"},"path":{"type":"object","properties":{"id":{"type":"integer"},"label":{"type":"string"}}}}}`, args: []string{"--path.id=literal", "--id=top", "--label=child"}, want: `{"path.id":"literal","id":"top","path":{"label":"child"}}`},
		{name: "retained wrapper overlap", schema: `{"type":"object","properties":{"path.id":{"type":"string"},"id":{"type":"string"},"path":{"type":"object","properties":{"id":{"type":"integer"},"label":{"type":"string"}}}}}`, args: []string{"--path={}", "--label=child"}, invalid: true},
		{name: "retained wrapper overlap reversed", schema: `{"type":"object","properties":{"path.id":{"type":"string"},"id":{"type":"string"},"path":{"type":"object","properties":{"id":{"type":"integer"},"label":{"type":"string"}}}}}`, args: []string{"--label=child", "--path={}"}, invalid: true},
		{name: "mixed string branch", schema: `{"type":"object","properties":{"body":{"type":"object","oneOf":[{"properties":{"value":{"type":"string"}}},{"properties":{"value":{"type":"integer"}}}]}}}`, args: []string{`--value="literal"`}, want: `{"body":{"value":"literal"}}`},
		{name: "mixed number branch", schema: `{"type":"object","properties":{"body":{"type":"object","oneOf":[{"properties":{"value":{"type":"string"}}},{"properties":{"value":{"type":"integer"}}}]}}}`, args: []string{"--value=9007199254740993"}, want: `{"body":{"value":9007199254740993}}`},
		{name: "mixed literal text", schema: `{"type":"object","properties":{"body":{"type":"object","oneOf":[{"properties":{"value":{"type":"string"}}},{"properties":{"value":{"type":"integer"}}}]}}}`, args: []string{"--value=literal"}, want: `{"body":{"value":"literal"}}`, helpContains: []string{"--value <JSON or literal text>", "valid JSON keeps its type"}},
		{name: "mixed quoted numeric string", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"value":{"type":["string","integer"]}}}}}`, args: []string{`--value="4"`}, want: `{"body":{"value":"4"}}`},
		{name: "mixed malformed structure as text", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"value":{"type":["string","object"]}}}}}`, args: []string{`--value={unfinished`}, want: `{"body":{"value":"{unfinished"}}`},
		{name: "mixed valid JSON never reinterpreted", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"value":{"type":["string","integer"],"not":{"const":4}}}}}}`, args: []string{"--value=4"}, invalid: true},
		{name: "mixed duplicate JSON rejected", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"value":{"type":["string","object"]}}}}}`, args: []string{`--value={"a":1,"a":2}`}, invalid: true},
		{name: "allOf discovery", schema: `{"type":"object","allOf":[{"properties":{"body":{"type":"object","allOf":[{"properties":{"text":{"type":"string"}}},{"properties":{"count":{"type":"integer"}}}]}}}]}`, args: []string{"--text=literal", "--count=2"}, want: `{"body":{"text":"literal","count":2}}`},
		{name: "anyOf branch names", schema: `{"type":"object","properties":{"body":{"type":"object","anyOf":[{"properties":{"first":{"type":"string"}},"required":["first"]},{"properties":{"second":{"type":"integer"}},"required":["second"]}]}}}`, args: []string{"--first=literal", "--second=2"}, want: `{"body":{"first":"literal","second":2}}`},
		{name: "optional omitted default", schema: `{"type":"object","properties":{"body":{"type":"object","properties":{"text":{"type":"string","default":"provider"}},"required":["text"]}}}`, want: `{}`},
		{name: "cyclic value JSON", schema: `{"type":"object","properties":{"body":{"$ref":"#/$defs/node"}},"$defs":{"node":{"type":"object","properties":{"next":{"$ref":"#/$defs/node"},"text":{"type":"string"}}}}}`, args: []string{`--next={"text":"nested"}`}, want: `{"body":{"next":{"text":"nested"}}}`},
		{name: "conditional text", schema: conditional, args: []string{"--mode=text", `--value="text"`, "--detail=literal"}, want: `{"body":{"mode":"text","value":"text","detail":"literal"}}`},
		{name: "conditional integer", schema: conditional, args: []string{"--mode=number", "--value=4", "--count=2"}, want: `{"body":{"mode":"number","value":4,"count":2}}`},
		{name: "conditionally required", schema: conditional, args: []string{"--mode=text", `--value="text"`}, invalid: true},
		{name: "prohibited combination", schema: conditional, args: []string{"--mode=text", `--value="text"`, "--detail=literal", "--count=2"}, invalid: true},
		{name: "branch type", schema: conditional, args: []string{"--mode=number", `--value="text"`, "--count=2"}, invalid: true},
		{name: "gateway Gitea", fixture: "issueCreateComment", args: []string{"--owner=alice", "--repo=demo", "--index=12", "--body=literal comment"}, want: `{"path":{"owner":"alice","repo":"demo","index":12},"body":{"body":"literal comment"}}`},
		{name: "gateway crawl defaults omitted", fixture: "crawl_markdown", args: []string{"--url=https://example.test"}, want: `{"body":{"url":"https://example.test"}}`},
		{name: "gateway conversion", fixture: "submit_document_conversion", args: []string{"--source_url=https://example.test/dummy.pdf", "--force_ocr=false", "--page_end=3"}, want: `{"body":{"source_url":"https://example.test/dummy.pdf","force_ocr":false,"page_end":3}}`},
		{name: "gateway PDF JSON parameters", fixture: "edit_pdf", args: []string{`--source={"base64_string":"U1lOVEhFVElD"}`, `--edits=[{"type":"text","page":1,"rect":{"x":0,"y":0,"width":100,"height":30},"text":"DUMMY 2026-10-17"}]`}, want: `{"body":{"source":{"base64_string":"U1lOVEhFVElD"},"edits":[{"type":"text","page":1,"rect":{"x":0,"y":0,"width":100,"height":30},"text":"DUMMY 2026-10-17"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fixture != "" {
				data, err := os.ReadFile(filepath.Join("testdata", "cli-schemas", tc.fixture+".json"))
				require.NoError(t, err)
				tc.schema = string(data)
			}
			var calls atomic.Int32
			received := make(chan json.RawMessage, 1)
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "projection", Version: "1"}, nil)
			server.AddTool(&mcpsdk.Tool{Name: "record", InputSchema: json.RawMessage(tc.schema)}, func(_ context.Context, request *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				calls.Add(1)
				received <- bytes.Clone(request.Params.Arguments)
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{}}, nil
			})
			remote := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
			defer remote.Close()
			binding := filepath.Join(t.TempDir(), "binding.json")
			require.NoError(t, os.WriteFile(binding, []byte(`{"name":"fixture","http":{"params":{"url":"`+remote.URL+`"}}}`), 0600))
			cmd := exec.Command(binary, append([]string{"--binding-file", binding, "record"}, tc.args...)...)
			cmd.Stdin = strings.NewReader(tc.input)
			output, err := cmd.CombinedOutput()
			if tc.invalid {
				require.Error(t, err, "%s", output)
				require.Zero(t, calls.Load())
				if tc.schema == conditional {
					require.Contains(t, string(output), "validation failed")
				}
				return
			}
			require.NoError(t, err, "%s", output)
			require.Equal(t, int32(1), calls.Load())
			got := <-received
			require.JSONEq(t, tc.want, string(got))
			if strings.Contains(tc.want, "9007199254740993") {
				require.Contains(t, string(got), "9007199254740993")
			}
			for range max(1, tc.helpRepeats) {
				cmd = exec.Command(binary, "--binding-file", binding, "record", "--help")
				output, err = cmd.CombinedOutput()
				require.NoError(t, err, "%s", output)
				require.NotContains(t, string(output), `"properties"`)
				require.Contains(t, string(output), "--help schema")
				for _, argument := range tc.args {
					name, _, _ := strings.Cut(argument, "=")
					require.Contains(t, string(output), name)
				}
				for _, expected := range tc.helpContains {
					require.Contains(t, string(output), expected)
				}
				for _, absent := range tc.helpAbsent {
					require.NotContains(t, string(output), absent)
				}
			}
			cmd = exec.Command(binary, "--binding-file", binding, "record", "--help", "schema")
			output, err = cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.JSONEq(t, tc.schema, string(output))
			require.Equal(t, int32(1), calls.Load(), "help never invokes")
		})
	}
}
