package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"dagger.io/dagger"
)

const workspaceIDEnv = "DAGGER_GO_WORKSPACE_ID"

// currentWorkspace is a temporary shim for dag.CurrentWorkspace, which exists
// but is not usable here yet.
func currentWorkspace(ctx context.Context) (*dagger.Workspace, error) {
	rawID := os.Getenv(workspaceIDEnv)
	if rawID == "" {
		return nil, fmt.Errorf("%s is not set", workspaceIDEnv)
	}
	workspaceID := rawID
	if err := json.Unmarshal([]byte(rawID), &workspaceID); err != nil {
		workspaceID = rawID
	}

	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return nil, err
	}
	return dagger.Ref[*dagger.Workspace](client, dagger.ID(workspaceID)), nil
}
