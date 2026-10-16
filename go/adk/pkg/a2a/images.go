package a2a

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// resumeImageParts keeps actual images outside confirmation/answer JSON. The
// bridge validates decoding; this native boundary also bounds raw byte custody.
func resumeImageParts(incoming *a2atype.Message) ([]*a2atype.Part, error) {
	var images []*a2atype.Part
	total := 0
	for _, part := range incoming.Parts {
		converted, err := a2aPartConverter(context.Background(), nil, part)
		if err != nil {
			return nil, err
		}
		if converted == nil || converted.InlineData == nil {
			continue
		}
		blob := converted.InlineData
		total += len(blob.Data)
		if (blob.MIMEType != "image/jpeg" && blob.MIMEType != "image/png") || len(blob.Data) == 0 || total > 3*1024*1024 || len(images) == 2 {
			return nil, fmt.Errorf("ask_user images require at most two JPEG/PNG originals totaling 3 MiB")
		}
		images = append(images, part)
	}
	return images, nil
}

// newHITLImagesPlugin retains actual images after the resumed ask_user result.
// Earlier placement is lost by ADK's async function-response rearrangement;
// confirmation-bearing events themselves are excluded from model contents.
func newHITLImagesPlugin(service adksession.Service) (*plugin.Plugin, error) {
	return (&hitlImageState{service: service}).plugin()
}

type hitlImageState struct {
	service adksession.Service
	active  sync.Map
}

func (s *hitlImageState) plugin() (*plugin.Plugin, error) {
	return plugin.New(plugin.Config{Name: "kagent-hitl-images", BeforeRunCallback: s.beforeRun, AfterRunCallback: s.afterRun, BeforeModelCallback: s.beforeModel})
}
func (s *hitlImageState) beforeRun(ctx adkagent.InvocationContext) (*genai.Content, error) {
	if current := ctx.Session(); current != nil {
		s.active.Store(ctx.InvocationID(), current)
	}
	return nil, nil
}
func (s *hitlImageState) afterRun(ctx adkagent.InvocationContext) {
	s.active.Delete(ctx.InvocationID())
}

func (s *hitlImageState) beforeModel(ctx adkagent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
	incoming := ctx.UserContent()
	if incoming == nil {
		return nil, nil
	}
	confirmation := false
	var images []*genai.Part
	for _, part := range incoming.Parts {
		if part == nil {
			continue
		}
		if part.FunctionResponse != nil && part.FunctionResponse.Name == toolconfirmation.FunctionCallName {
			confirmation = true
		}
		if part.InlineData != nil {
			images = append(images, part)
		}
	}
	if !confirmation || len(images) == 0 {
		return nil, nil
	}
	// The executor preserves images only for direct ask_user continuations.
	// Wait for that tool's real result; other tools/agents do not own this input.
	resultPresent := false
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == "ask_user" {
				resultPresent = true
			}
		}
	}
	if !resultPresent {
		return nil, nil
	}
	if s.service == nil {
		return nil, fmt.Errorf("ask_user image continuation requires native session custody")
	}
	eventID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("kagent-hitl-images/"+ctx.InvocationID())).String()
	captured, ok := s.active.Load(ctx.InvocationID())
	if !ok {
		return nil, fmt.Errorf("active image continuation session is missing")
	}
	current := captured.(adksession.Session)
	if current.AppName() != ctx.AppName() || current.UserID() != ctx.UserID() || current.ID() != ctx.SessionID() {
		return nil, fmt.Errorf("image continuation does not match active session")
	}
	// BeforeRun exposes the actual runner snapshot; a separate Get returns
	// another optimistic-update timestamp. Appending against that other
	// snapshot invalidates the runner's later writes to persistent SQLite.
	retained := false
	isolationScope := ""
	for event := range current.Events().All() {
		if event.ID == eventID {
			retained = true
		}
		// The callback exposes no IsolationScope() in pinned ADK. The runner's
		// current user event carries the authoritative scope instead.
		if event.InvocationID == ctx.InvocationID() && event.Author == "user" {
			isolationScope = event.IsolationScope
		}
	}
	content := &genai.Content{Role: "user", Parts: images}
	event := &adksession.Event{ID: eventID, Author: "user", InvocationID: ctx.InvocationID(), Timestamp: time.Now(), Branch: ctx.Branch(), IsolationScope: isolationScope, LLMResponse: model.LLMResponse{Content: content}}
	if !retained {
		if err := s.service.AppendEvent(ctx, current, event); err != nil {
			return nil, fmt.Errorf("retain native image continuation: %w", err)
		}
	}
	// This request was assembled before the append. Add actual images now;
	// later requests and fresh runs consume the same retained native event.
	if !retained || !hasImageContent(request.Contents, images) {
		request.Contents = append(request.Contents, content)
	}
	return nil, nil
}

func hasImageContent(contents []*genai.Content, images []*genai.Part) bool {
	for _, content := range contents {
		if content == nil || content.Role != "user" || len(content.Parts) != len(images) {
			continue
		}
		match := true
		for index, part := range content.Parts {
			want := images[index].InlineData
			if part == nil || part.InlineData == nil || part.InlineData.MIMEType != want.MIMEType || !bytes.Equal(part.InlineData.Data, want.Data) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
