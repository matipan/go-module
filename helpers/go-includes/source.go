package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const sourceRoot = "/src"

func sourceFileContents(_ context.Context, filePath string) ([]byte, error) {
	cleanPath := cleanWorkspacePath(filePath)
	if escapesWorkspace(cleanPath) {
		return nil, fmt.Errorf("path escapes workspace: %s", filePath)
	}
	return os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(cleanPath)))
}
