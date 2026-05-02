package main

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
)

type workspaceGlobber func(context.Context, string) ([]string, error)

type localGoModule struct {
	dir  string
	path string
}

func localImportIncludes(ctx context.Context, seedGoMods, seedGoFiles []string, readFile goModReader, glob workspaceGlobber) ([]string, error) {
	modules, err := localGoModules(ctx, seedGoMods, readFile)
	if err != nil {
		return nil, err
	}

	var includes []string
	seenIncludes := map[string]bool{}
	seenFiles := map[string]bool{}

	queue := cleanWorkspacePaths(seedGoFiles)
	for len(queue) > 0 {
		filePath := queue[0]
		queue = queue[1:]
		if seenFiles[filePath] {
			continue
		}
		seenFiles[filePath] = true

		data, err := readFile(ctx, filePath)
		if err != nil {
			// Dagger glob can match directories whose names end in .go; Go ignores them.
			if strings.Contains(err.Error(), "is a directory, not a file") {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", filePath, err)
		}
		imports, err := goFileImports(filePath, data)
		if err != nil {
			return nil, err
		}
		for _, importPath := range imports {
			pkgDir, ok := localImportDir(modules, importPath)
			if !ok || escapesWorkspace(pkgDir) {
				continue
			}
			include := packageInclude(pkgDir)
			if !seenIncludes[include] {
				seenIncludes[include] = true
				includes = append(includes, include)
			}
			files, err := glob(ctx, packageGoFiles(pkgDir))
			if err != nil {
				return nil, err
			}
			queue = append(queue, cleanWorkspacePaths(files)...)
		}
	}

	return includes, nil
}

func localGoModules(ctx context.Context, goModPaths []string, readFile goModReader) ([]localGoModule, error) {
	var modules []localGoModule
	for _, goModPath := range cleanWorkspacePaths(goModPaths) {
		data, err := readFile(ctx, goModPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", goModPath, err)
		}
		file, err := modfile.Parse(goModPath, data, nil)
		if err != nil {
			return nil, err
		}
		if file.Module == nil {
			continue
		}
		modules = append(modules, localGoModule{
			dir:  path.Dir(goModPath),
			path: file.Module.Mod.Path,
		})
	}
	sort.Slice(modules, func(i, j int) bool {
		return len(modules[i].path) > len(modules[j].path)
	})
	return modules, nil
}

func goFileImports(filePath string, data []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filePath, data, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}

	imports := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		imports = append(imports, importPath)
	}
	return imports, nil
}

func localImportDir(modules []localGoModule, importPath string) (string, bool) {
	for _, module := range modules {
		if importPath != module.path && !strings.HasPrefix(importPath, module.path+"/") {
			continue
		}
		suffix := strings.TrimPrefix(importPath, module.path)
		suffix = strings.TrimPrefix(suffix, "/")
		if suffix == "" {
			return module.dir, true
		}
		return addIncludePrefix(module.dir, suffix), true
	}
	return "", false
}

func packageInclude(pkgDir string) string {
	if pkgDir == "" || pkgDir == "." {
		return "*"
	}
	return pkgDir + "/**"
}

func packageGoFiles(pkgDir string) string {
	if pkgDir == "" || pkgDir == "." {
		return "*.go"
	}
	return pkgDir + "/*.go"
}

func cleanWorkspacePaths(paths []string) []string {
	cleaned := make([]string, 0, len(paths))
	for _, filePath := range paths {
		cleaned = append(cleaned, cleanWorkspacePath(filePath))
	}
	return cleaned
}
