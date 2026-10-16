package a2a

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestHITLImageSnapshotOwnershipAndRunCleanup(t *testing.T) {
	for _, scenario := range []string{"completed", "agent error", "consumer interrupted", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			sessions := session.InMemoryService()
			state := &hitlImageState{service: sessions}
			imagePlugin, err := state.plugin()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := false
			actor, err := agent.New(agent.Config{Name: "lifecycle-probe", Run: func(ic agent.InvocationContext) iter.Seq2[*session.Event, error] {
				return func(yield func(*session.Event, error) bool) {
					entered = true
					captured, ok := state.active.Load(ic.InvocationID())
					if !ok || captured.(session.Session) != ic.Session() {
						t.Fatal("plugin did not capture the runner's actual session snapshot")
					}
					if scenario == "agent error" {
						yield(nil, errors.New("synthetic agent failure"))
						return
					}
					if scenario == "cancelled" {
						cancel()
						yield(nil, ctx.Err())
						return
					}
					for range 2 {
						event := session.NewEvent(ic, ic.InvocationID())
						event.Author = ic.Agent().Name()
						event.LLMResponse = model.LLMResponse{Content: genai.NewContentFromText("fixture", "model")}
						if !yield(event, nil) {
							return
						}
					}
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := runner.New(runner.Config{AppName: "lifecycle", Agent: actor, SessionService: sessions, PluginConfig: runner.PluginConfig{Plugins: []*plugin.Plugin{imagePlugin}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, identity := range []struct{ user, id string }{{"first-user", "first-session"}, {"second-user", "second-session"}} {
				if _, err := sessions.Create(t.Context(), &session.CreateRequest{AppName: "lifecycle", UserID: identity.user, SessionID: identity.id}); err != nil {
					t.Fatal(err)
				}
				entered = false
				for _, err := range runtime.Run(ctx, identity.user, identity.id, genai.NewContentFromText("fixture input", "user"), agent.RunConfig{}) {
					if err != nil && scenario != "agent error" && scenario != "cancelled" {
						t.Fatal(err)
					}
					if scenario == "consumer interrupted" {
						break
					}
				}
				if !entered {
					t.Fatal("runner never entered the registered invocation")
				}
				active := 0
				state.active.Range(func(_, _ any) bool { active++; return true })
				if active != 0 {
					t.Fatalf("run retained %d session snapshots", active)
				}
				if scenario == "cancelled" {
					break
				}
			}
		})
	}
}
