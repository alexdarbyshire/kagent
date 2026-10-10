package translator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/stretchr/testify/require"
)

func TestBootstrapCapabilitiesReachActorAndRevisionIdentity(t *testing.T) {
	harness, template := cliConfiguration()
	template.Spec.Tools = nil
	raw, err := json.Marshal(harness)
	require.NoError(t, err)
	var declaration map[string]any
	require.NoError(t, json.Unmarshal(raw, &declaration))
	declaration["spec"].(map[string]any)["workload"].(map[string]any)["capabilities"] = map[string]any{"drop": []string{"ALL"}, "add": []string{"CHOWN", "SETUID", "SETGID", "NET_BIND_SERVICE"}}
	raw, err = json.Marshal(declaration)
	require.NoError(t, err)
	var configured v1alpha3.Harness
	require.NoError(t, json.Unmarshal(raw, &configured))
	baseline, err := compiler(t, modelConfig()).CompileAgent(context.Background(), inlineAgent(harness, template))
	require.NoError(t, err)
	changed, err := compiler(t, modelConfig()).CompileAgent(context.Background(), inlineAgent(&configured, template))
	require.NoError(t, err)
	oldID, err := baseline.Digest()
	require.NoError(t, err)
	newID, err := changed.Digest()
	require.NoError(t, err)
	require.NotEqual(t, oldID, newID)
	actor, err := substrate.ActorTemplateForRevision(&changed.Revision, newID)
	require.NoError(t, err)
	capabilities := actor.Containers[0].GetSecurityContext().GetCapabilities()
	require.Equal(t, []string{"ALL"}, capabilities.GetDrop())
	require.Equal(t, []string{"CHOWN", "NET_BIND_SERVICE", "SETGID", "SETUID"}, capabilities.GetAdd())
}
