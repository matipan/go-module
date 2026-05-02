package main

import (
	"context"
	"encoding/json"
	"os"

	"dagger.io/dagger"
	"dagger.io/dagger/dag"
)

const workspaceIDEnv = "DAGGER_GO_HELPER_WORKSPACE_ID"

func currentWorkspace() *dagger.Workspace {
	rawID := os.Getenv(workspaceIDEnv)
	if rawID == "" {
		panic(workspaceIDEnv + " must be set")
	}
	var id string
	if err := json.Unmarshal([]byte(rawID), &id); err != nil {
		panic(err)
	}
	return dag.LoadWorkspaceFromID(dagger.WorkspaceID(id))
}

func workspaceGoModContents(ctx context.Context, goModPath string) ([]byte, error) {
	contents, err := currentWorkspace().
		Directory("/", dagger.WorkspaceDirectoryOpts{Include: []string{goModPath}}).
		File(goModPath).
		Contents(ctx)
	if err != nil {
		return nil, err
	}
	return []byte(contents), nil
}
