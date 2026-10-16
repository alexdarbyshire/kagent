package nativeartifacts

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewHTTPTransport creates the investigator's request-response MCP transport.
// Optional standalone GET streams stay disabled; POST responses may still stream.
// Discovery alone recognizes the pinned gateway's expired UUID response, allowing
// ADK's existing bounded reconnect. Tool calls are never replayed by this adapter.
func NewHTTPTransport(endpoint string, client *http.Client) *mcp.StreamableClientTransport {
	copied := http.Client{}
	if client != nil {
		copied = *client
	}
	base := copied.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copied.Transport = &discoveryTransport{endpoint: endpoint, base: base}
	return &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &copied, DisableStandaloneSSE: true}
}

type discoveryTransport struct {
	endpoint string
	base     http.RoundTripper
}

func (t *discoveryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return response, err
	}
	if response.StatusCode != http.StatusBadRequest || req.Method != http.MethodPost || req.URL.String() != t.endpoint || req.GetBody == nil {
		return response, nil
	}
	if _, err := uuid.Parse(req.Header.Get("Mcp-Session-Id")); err != nil {
		return response, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return response, nil
	}
	var message struct {
		Method string `json:"method"`
	}
	decodeErr := json.NewDecoder(body).Decode(&message)
	closeErr := body.Close()
	if decodeErr != nil || closeErr != nil || message.Method != "tools/list" {
		return response, nil
	}
	// Gateway v1.6.0 returns this plain body instead of MCP's required 404 after
	// reaping a stateful UUID. Leave all other responses and methods unchanged.
	expected := []byte("mcp: invalid session ID header")
	prefix, readErr := io.ReadAll(io.LimitReader(response.Body, int64(len(expected)+1)))
	if readErr != nil || !bytes.Equal(prefix, expected) {
		response.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
		return response, nil
	}
	response.Body = &replayBody{Reader: bytes.NewReader(prefix), Closer: response.Body}
	response.StatusCode = http.StatusNotFound
	response.Status = "404 Not Found"
	return response, nil
}

type replayBody struct {
	io.Reader
	io.Closer
}
