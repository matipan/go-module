package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"dagger.io/dagger"
)

const workspaceIDEnv = "DAGGER_GO_WORKSPACE_ID"

var daggerClient *dagger.Client

func currentWorkspace(ctx context.Context) (*dagger.Workspace, error) {
	rawID := os.Getenv(workspaceIDEnv)
	if rawID == "" {
		return nil, fmt.Errorf("%s is not set", workspaceIDEnv)
	}
	workspaceID := rawID
	if err := json.Unmarshal([]byte(rawID), &workspaceID); err != nil {
		workspaceID = rawID
	}

	var err error
	if daggerClient == nil {
		daggerClient, err = dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
		if err != nil {
			return nil, err
		}
	}
	return daggerClient.LoadWorkspaceFromID(dagger.WorkspaceID(workspaceID)), nil
}

func closeDaggerClient() error {
	if daggerClient == nil {
		return nil
	}
	err := daggerClient.Close()
	daggerClient = nil
	return err
}

type daggerWorkspace struct {
	ws *dagger.Workspace
}

func (w daggerWorkspace) directory(include, exclude []string) workspaceDirectory {
	return daggerWorkspaceDirectory{dir: w.ws.Directory("/", dagger.WorkspaceDirectoryOpts{
		Include: include,
		Exclude: exclude,
	})}
}

type daggerWorkspaceDirectory struct {
	dir *dagger.Directory
}

func (d daggerWorkspaceDirectory) glob(ctx context.Context, pattern string) ([]string, error) {
	return d.dir.Glob(ctx, pattern)
}

func (d daggerWorkspaceDirectory) readFile(ctx context.Context, filePath string) ([]byte, error) {
	cleanPath := cleanWorkspacePath(filePath)
	if escapesWorkspace(cleanPath) {
		return nil, fmt.Errorf("path escapes workspace: %s", filePath)
	}
	contents, err := d.dir.File(cleanPath).Contents(ctx)
	if err != nil {
		fileType, statErr := d.dir.Stat(cleanPath).FileType(ctx)
		if statErr == nil && fileType == dagger.FileTypeDirectory {
			return nil, errNotRegularFile
		}
		return nil, err
	}
	return []byte(contents), nil
}
