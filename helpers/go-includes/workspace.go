package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"dagger.io/dagger"
	"dagger.io/dagger/dag"
)

const workspaceIDEnv = "DAGGER_GO_HELPER_WORKSPACE_ID"
const sourceRoot = "/src"

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

func sourceFileContents(_ context.Context, filePath string) ([]byte, error) {
	cleanPath := cleanWorkspacePath(filePath)
	if escapesWorkspace(cleanPath) {
		return nil, fmt.Errorf("path escapes workspace: %s", filePath)
	}
	return os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(cleanPath)))
}

func sourceGlob(_ context.Context, pattern string) ([]string, error) {
	cleanPattern := cleanWorkspacePath(pattern)
	if escapesWorkspace(cleanPattern) {
		return nil, fmt.Errorf("pattern escapes workspace: %s", pattern)
	}
	matches, err := filepath.Glob(filepath.Join(sourceRoot, filepath.FromSlash(cleanPattern)))
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		rel, err := filepath.Rel(sourceRoot, match)
		if err != nil {
			return nil, err
		}
		paths = append(paths, filepath.ToSlash(rel))
	}
	return paths, nil
}
