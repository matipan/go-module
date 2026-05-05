package main

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"

	telemetry "github.com/dagger/otel-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type workspaceRoot interface {
	directory(include, exclude []string) workspaceDirectory
}

type workspaceDirectory interface {
	glob(ctx context.Context, pattern string) ([]string, error)
	readFile(ctx context.Context, filePath string) ([]byte, error)
	search(ctx context.Context, pattern string, globs []string) ([]string, error)
}

var errNotRegularFile = errors.New("not a regular file")

type moduleDiscovery struct {
	workspace  workspaceRoot
	modulePath string
	recursive  bool
}

func (d moduleDiscovery) includes(ctx context.Context) (_ []string, rerr error) {
	var all []string
	allSeen := map[string]bool{}
	addIncludes(&all, allSeen, d.includeBase()...)

	var discovered []string
	discoveredSeen := map[string]bool{}
	directiveIncludes, err := d.directiveIncludes(ctx)
	if err != nil {
		return nil, err
	}
	addIncludes(&all, allSeen, directiveIncludes...)
	addIncludes(&discovered, discoveredSeen, directiveIncludes...)

	replaceIncludes, err := d.goModReplaceIncludes(ctx, all, allSeen)
	if err != nil {
		return nil, err
	}
	addIncludes(&discovered, discoveredSeen, replaceIncludes...)
	return discovered, nil
}

func (d moduleDiscovery) includeBase() []string {
	patterns := []string{
		"**/*.go",
		"**/*.c",
		"**/*.h",
		"**/*.s",
		"**/*.S",
		"**/*.syso",
		"**/go.mod",
		"**/go.sum",
		"**/go.work",
		"**/go.work.sum",
	}
	for i, pattern := range patterns {
		patterns[i] = addIncludePrefix(d.modulePath, pattern)
	}
	return patterns
}

func (d moduleDiscovery) directiveIncludes(ctx context.Context) (_ []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes scan directives")
	defer telemetry.EndWithCause(span, &rerr)

	excludes, err := d.nestedModuleExcludes(ctx)
	if err != nil {
		return nil, err
	}
	dir := d.workspace.directory([]string{addIncludePrefix(d.modulePath, "**/*.go")}, excludes)
	goFiles, err := dir.glob(ctx, "**/*.go")
	if err != nil {
		return nil, err
	}
	sort.Strings(goFiles)

	var includes []string
	seen := map[string]bool{}
	for _, filePath := range goFiles {
		data, err := dir.readFile(ctx, filePath)
		if errors.Is(err, errNotRegularFile) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fileIncludes, err := goDirectiveIncludesFromBytes(filePath, includePrefixForGoFile(filePath), data)
		if err != nil {
			return nil, err
		}
		addIncludes(&includes, seen, fileIncludes...)
	}
	span.SetAttributes(
		attribute.Int("go_includes.file_count", len(goFiles)),
		attribute.Int("go_includes.include_count", len(includes)),
	)
	return includes, nil
}

func (d moduleDiscovery) nestedModuleExcludes(ctx context.Context) ([]string, error) {
	goMods, err := goModPaths(ctx, d.workspace)
	if err != nil {
		return nil, err
	}
	var excludes []string
	for _, goModPath := range goMods {
		nestedPath := "."
		if goModPath != "go.mod" {
			nestedPath = strings.TrimSuffix(goModPath, "/go.mod")
		}
		if d.modulePath == "." {
			if nestedPath != "." {
				excludes = append(excludes, nestedPath+"/**")
			}
			continue
		}
		if nestedPath != d.modulePath && strings.HasPrefix(nestedPath, strings.TrimSuffix(d.modulePath, "/")+"/") {
			excludes = append(excludes, nestedPath+"/**")
		}
	}
	sort.Strings(excludes)
	return excludes, nil
}

func (d moduleDiscovery) goModReplaceIncludes(ctx context.Context, all []string, allSeen map[string]bool) (_ []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes scan go.mod replaces")
	defer telemetry.EndWithCause(span, &rerr)

	seenGoMods := map[string]bool{}
	seenIncludes := map[string]bool{}
	var includes []string
	passCount := 0
	for {
		dir := d.workspace.directory(all, nil)
		goMods, err := dir.glob(ctx, "**/go.mod")
		if err != nil {
			return nil, err
		}
		sort.Strings(goMods)

		var newGoMods []string
		for _, goModPath := range goMods {
			goModPath = cleanWorkspacePath(goModPath)
			if seenGoMods[goModPath] {
				continue
			}
			seenGoMods[goModPath] = true
			newGoMods = append(newGoMods, goModPath)
		}
		if len(newGoMods) == 0 {
			break
		}

		passCount++
		for _, goModPath := range newGoMods {
			data, err := dir.readFile(ctx, goModPath)
			if err != nil {
				return nil, err
			}
			replaces, err := goModLocalReplaceIncludes(goModPath, data)
			if err != nil {
				return nil, err
			}
			for _, replace := range replaces {
				addIncludes(&all, allSeen, replace.include)
				addIncludes(&includes, seenIncludes, replace.include)
			}
		}
		if !d.recursive {
			break
		}
	}
	span.SetAttributes(
		attribute.Int("go_includes.go_mod_count", len(seenGoMods)),
		attribute.Int("go_includes.pass_count", passCount),
		attribute.Int("go_includes.include_count", len(includes)),
	)
	return includes, nil
}

func generateModules(ctx context.Context, workspace workspaceRoot, onlyModule string) ([]string, error) {
	goMods, err := goModPaths(ctx, workspace)
	if err != nil {
		return nil, err
	}

	moduleSet := map[string]bool{}
	for _, goModPath := range goMods {
		moduleSet[modulePathForGoMod(goModPath)] = true
	}

	include := []string{"**/*.go"}
	if onlyModule != "" {
		include = []string{addIncludePrefix(onlyModule, "**/*.go")}
	}
	goFiles, err := workspace.directory(include, nil).search(ctx, "//go:generate", []string{"**/*.go"})
	if err != nil {
		return nil, err
	}

	hasGenerate := map[string]bool{}
	for _, goFile := range goFiles {
		modulePath, ok := containingModule(goFile, moduleSet)
		if !ok {
			continue
		}
		if onlyModule != "" && modulePath != onlyModule {
			continue
		}
		hasGenerate[modulePath] = true
	}

	var modules []string
	for _, goModPath := range goMods {
		modulePath := modulePathForGoMod(goModPath)
		if hasGenerate[modulePath] {
			modules = append(modules, modulePath)
		}
	}
	return modules, nil
}

func goModPaths(ctx context.Context, workspace workspaceRoot) ([]string, error) {
	goMods, err := workspace.directory([]string{"**/go.mod"}, nil).glob(ctx, "**/go.mod")
	if err != nil {
		return nil, err
	}
	sort.Strings(goMods)
	return goMods, nil
}

func modulePathForGoMod(goModPath string) string {
	if goModPath == "go.mod" {
		return "."
	}
	return strings.TrimSuffix(goModPath, "/go.mod")
}

func containingModule(filePath string, moduleSet map[string]bool) (string, bool) {
	dir := path.Dir(cleanWorkspacePath(filePath))
	for {
		if moduleSet[dir] {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

func addIncludes(list *[]string, seen map[string]bool, patterns ...string) {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if seen[pattern] {
			continue
		}
		seen[pattern] = true
		*list = append(*list, pattern)
	}
}
