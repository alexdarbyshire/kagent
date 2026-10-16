// Package nativeartifacts adapts raw MCP images to session-scoped ADK artifacts.
package nativeartifacts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"
)

// ProcessService is shared by all runners in this actor process.
// Its session-scoped captures intentionally do not survive a process restart.
func ProcessService() artifact.Service { return processArtifacts }

var processArtifacts = artifact.InMemoryService()

// TransformResult replaces image data with session-scoped references while
// preserving every other wire field, including exact JSON numbers.
func TransformResult(ctx context.Context, data json.RawMessage) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("decode MCP result: %w", err)
	}
	var content []json.RawMessage
	if err := json.Unmarshal(envelope["content"], &content); err != nil {
		return data, nil
	}
	type capture struct {
		index int
		mime  string
		data  []byte
	}
	var captures []capture
	total := 0
	for index, raw := range content {
		var block struct {
			Type string `json:"type"`
			MIME string `json:"mimeType"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return nil, err
		}
		if block.Type != "image" {
			continue
		}
		if block.MIME != "image/png" && block.MIME != "image/jpeg" {
			return nil, fmt.Errorf("unsupported MCP image MIME type %q", block.MIME)
		}
		encodedLength := 0
		var last, previous byte
		for index := 0; index < len(block.Data); index++ {
			value := block.Data[index]
			if value == '\r' || value == '\n' {
				continue
			}
			encodedLength++
			previous, last = last, value
		}
		decodedSize := base64.StdEncoding.DecodedLen(encodedLength)
		if last == '=' {
			decodedSize--
		}
		if previous == '=' {
			decodedSize--
		}
		if decodedSize > 3*1024*1024-total {
			return nil, fmt.Errorf("MCP images exceed 3 MiB aggregate decoded-image budget; no artifacts saved")
		}
		decoded, err := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(block.Data)), int64(3*1024*1024-total+1)))
		if err != nil || len(decoded) == 0 {
			return nil, fmt.Errorf("invalid MCP image data")
		}
		total += len(decoded)
		if total > 3*1024*1024 {
			return nil, fmt.Errorf("MCP images exceed 3 MiB aggregate decoded-image budget; no artifacts saved")
		}
		captures = append(captures, capture{index: index, mime: block.MIME, data: decoded})
	}
	if len(captures) == 0 {
		return data, nil
	}
	ac, ok := ctx.(agent.Context)
	if !ok || ac == nil {
		return nil, fmt.Errorf("MCP image has no invocation artifact context")
	}
	// Validate the entire batch before the first Save, including later images.
	for _, capture := range captures {
		_, format, err := image.Decode(bytes.NewReader(capture.data))
		expected := "png"
		if capture.mime == "image/jpeg" {
			expected = "jpeg"
		}
		if err != nil {
			return nil, fmt.Errorf("invalid MCP %s image; no artifacts saved: %w", capture.mime, err)
		}
		if format != expected {
			return nil, fmt.Errorf("invalid MCP %s image; no artifacts saved", capture.mime)
		}
	}
	refs := make([]any, 0, len(captures))
	for _, capture := range captures {
		extension := ".png"
		if capture.mime == "image/jpeg" {
			extension = ".jpg"
		}
		filename := "screenshot-" + uuid.NewString() + extension
		saved, err := ac.Artifacts().Save(ctx, filename, genai.NewPartFromBytes(capture.data, capture.mime))
		if err != nil {
			return nil, fmt.Errorf("save MCP image artifact (operation not replayed): %w", err)
		}
		refs = append(refs, map[string]any{"filename": filename, "mime_type": capture.mime, "version": saved.Version, "retention": "actor process memory; lost on restart"})
		content[capture.index], err = json.Marshal(&mcp.TextContent{Text: fmt.Sprintf("Screenshot artifact %s (%s, version %d). Use load_artifacts to view pixels. Stored only in this actor process; recapture after restart.", filename, capture.mime, saved.Version)})
		if err != nil {
			return nil, err
		}
	}
	adapted, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	structured := envelope["structuredContent"]
	if len(structured) == 0 {
		structured = json.RawMessage("null")
	}
	envelope["structuredContent"], err = json.Marshal(map[string]any{"result": structured, "content": content, "artifacts": refs})
	if err != nil {
		return nil, err
	}
	envelope["content"] = adapted
	return json.Marshal(envelope)
}
