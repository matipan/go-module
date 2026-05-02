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
	return workspaceFileContents(ctx, goModPath)
}

func workspaceFileContents(ctx context.Context, filePath string) ([]byte, error) {
	contents, err := currentWorkspace().
		Directory("/", dagger.WorkspaceDirectoryOpts{Include: []string{filePath}}).
		File(filePath).
		Contents(ctx)
	if err != nil {
		return nil, err
	}
	return []byte(contents), nil
}

func workspaceGlob(ctx context.Context, pattern string) ([]string, error) {
	return currentWorkspace().
		Directory("/", dagger.WorkspaceDirectoryOpts{Include: []string{pattern}}).
		Glob(ctx, pattern)
}

func workspaceGoSeeds(ctx context.Context, includePatterns []string) ([]string, []string, error) {
	source := currentWorkspace().Directory("/", dagger.WorkspaceDirectoryOpts{Include: includePatterns})
	goMods, err := source.Glob(ctx, "**/go.mod")
	if err != nil {
		return nil, nil, err
	}
	goFiles, err := source.Glob(ctx, "**/*.go")
	if err != nil {
		return nil, nil, err
	}
	return goMods, goFiles, nil
}
