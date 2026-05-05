package main

import (
	"path"
	"strings"

	"golang.org/x/mod/modfile"
)

type localReplaceInclude struct {
	include string
	goMod   string
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
