package main

import (
	"context"
	"fmt"
	"path"
	"strings"

	"golang.org/x/mod/modfile"
)

type goModReader func(context.Context, string) ([]byte, error)

type queuedGoMod struct {
	path     string
	required bool
}

type localReplaceInclude struct {
	include string
	goMod   string
}

func goModIncludes(ctx context.Context, seedGoMods []string, recursive bool, readGoMod goModReader) ([]string, error) {
	var includes []string
	seenIncludes := map[string]bool{}
	seenGoMods := map[string]bool{}

	queue := make([]queuedGoMod, 0, len(seedGoMods))
	for _, goModPath := range seedGoMods {
		queue = append(queue, queuedGoMod{
			path:     cleanWorkspacePath(goModPath),
			required: true,
		})
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		if seenGoMods[item.path] {
			continue
		}
		seenGoMods[item.path] = true

		data, err := readGoMod(ctx, item.path)
		if err != nil {
			if item.required {
				return nil, fmt.Errorf("read %s: %w", item.path, err)
			}
			continue
		}

		replaces, err := goModLocalReplaceIncludes(item.path, data)
		if err != nil {
			return nil, err
		}
		for _, replace := range replaces {
			if !seenIncludes[replace.include] {
				seenIncludes[replace.include] = true
				includes = append(includes, replace.include)
			}
			if recursive && !escapesWorkspace(replace.goMod) {
				queue = append(queue, queuedGoMod{path: replace.goMod})
			}
		}
	}

	return includes, nil
}

func goModLocalReplaceIncludes(goModPath string, data []byte) ([]localReplaceInclude, error) {
	file, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, err
	}

	var includes []localReplaceInclude
	for _, replace := range file.Replace {
		if replace.New.Version != "" || !isWorkspaceRelativePath(replace.New.Path) {
			continue
		}
		target := strings.TrimSuffix(replace.New.Path, "/")
		includes = append(includes, localReplaceInclude{
			include: addIncludePrefix(path.Dir(goModPath), target+"/**"),
			goMod:   cleanWorkspacePath(addIncludePrefix(path.Dir(goModPath), target+"/go.mod")),
		})
	}
	return includes, nil
}

func isWorkspaceRelativePath(path string) bool {
	return strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../")
}

func cleanWorkspacePath(filePath string) string {
	return path.Clean(strings.TrimPrefix(filePath, "/"))
}

func escapesWorkspace(filePath string) bool {
	return filePath == ".." || strings.HasPrefix(filePath, "../")
}
