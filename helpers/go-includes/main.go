// go.dang runs this helper to discover workspace include patterns.
// It emits one include pattern per line.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"dagger.io/dagger"
	telemetry "github.com/dagger/otel-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/mod/modfile"
)

var errNotRegularFile = errors.New("not a regular file")

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx := context.Background()
	cfg := telemetry.Config{}
	if exporter, ok := telemetry.ConfiguredSpanExporter(ctx); ok {
		cfg.LiveTraceExporters = append(cfg.LiveTraceExporters, exporter)
	}
	ctx = telemetry.Init(ctx, cfg)
	defer telemetry.Close()

	var (
		lines []string
		err   error
	)
	switch os.Args[1] {
	case "module":
		lines, err = runModule(ctx, os.Args[2:])
	case "generate-modules":
		lines, err = runGenerateModules(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, line := range lines {
		fmt.Println(line)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  go-includes module --path=DIR")
	fmt.Fprintln(os.Stderr, "  go-includes generate-modules [--path=DIR]")
}

func runModule(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes module")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags("module")
	modulePath := flags.String("path", "", "workspace-relative Go module root")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if *modulePath == "" {
		return nil, fmt.Errorf("--path is required")
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	ws, err := currentWorkspace(ctx)
	if err != nil {
		return nil, err
	}

	modulePathClean := cleanWorkspacePath(*modulePath)
	includes, rerr = moduleIncludes(ctx, ws, modulePathClean)
	span.SetAttributes(
		attribute.String("go_includes.module_path", modulePathClean),
		attribute.Int("go_includes.include_count", len(includes)),
	)
	return includes, rerr
}

func runGenerateModules(ctx context.Context, args []string) (modules []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes generate modules")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags("generate-modules")
	modulePath := flags.String("path", "", "only report this workspace-relative Go module root")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	ws, err := currentWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	onlyModule := *modulePath
	if onlyModule != "" {
		onlyModule = cleanWorkspacePath(onlyModule)
	}

	modules, rerr = generateModules(ctx, ws, onlyModule)
	span.SetAttributes(
		attribute.String("go_includes.module_path", onlyModule),
		attribute.Int("go_includes.module_count", len(modules)),
	)
	return modules, rerr
}

func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ExitOnError)
	flags.Usage = func() {
		usage()
		flags.PrintDefaults()
	}
	return flags
}

func workspaceDirectory(ws *dagger.Workspace, include, exclude []string) *dagger.Directory {
	return ws.Directory("/", dagger.WorkspaceDirectoryOpts{
		Include: append([]string(nil), include...),
		Exclude: append([]string(nil), exclude...),
	})
}

func readRegularFile(ctx context.Context, dir *dagger.Directory, filePath string) ([]byte, error) {
	cleanPath := cleanWorkspacePath(filePath)
	if escapesWorkspace(cleanPath) {
		return nil, fmt.Errorf("path escapes workspace: %s", filePath)
	}
	contents, err := dir.File(cleanPath).Contents(ctx)
	if err != nil {
		fileType, statErr := dir.Stat(cleanPath).FileType(ctx)
		if statErr == nil && fileType == dagger.FileTypeDirectory {
			return nil, errNotRegularFile
		}
		return nil, err
	}
	return []byte(contents), nil
}

func moduleIncludes(ctx context.Context, ws *dagger.Workspace, modulePath string) ([]string, error) {
	all := newIncludeSet(baseIncludes(modulePath)...)
	discovered := newIncludeSet()

	directives, err := directiveIncludes(ctx, ws, modulePath)
	if err != nil {
		return nil, err
	}
	all.add(directives...)
	discovered.add(directives...)

	replaces, err := goModReplaceIncludes(ctx, ws, all)
	if err != nil {
		return nil, err
	}
	discovered.add(replaces...)
	return discovered.list, nil
}

func baseIncludes(modulePath string) []string {
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
		patterns[i] = addIncludePrefix(modulePath, pattern)
	}
	return patterns
}

func directiveIncludes(ctx context.Context, ws *dagger.Workspace, modulePath string) (_ []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes scan directives")
	defer telemetry.EndWithCause(span, &rerr)

	excludes, err := nestedModuleExcludes(ctx, ws, modulePath)
	if err != nil {
		return nil, err
	}
	dir := workspaceDirectory(ws, []string{addIncludePrefix(modulePath, "**/*.go")}, excludes)
	goFiles, err := dir.Glob(ctx, "**/*.go")
	if err != nil {
		return nil, err
	}
	sort.Strings(goFiles)

	includes := newIncludeSet()
	for _, filePath := range goFiles {
		data, err := readRegularFile(ctx, dir, filePath)
		if errors.Is(err, errNotRegularFile) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fileIncludes, err := goDirectiveIncludesFromBytes(filePath, data)
		if err != nil {
			return nil, err
		}
		includes.add(fileIncludes...)
	}
	span.SetAttributes(
		attribute.Int("go_includes.file_count", len(goFiles)),
		attribute.Int("go_includes.include_count", len(includes.list)),
	)
	return includes.list, nil
}

func nestedModuleExcludes(ctx context.Context, ws *dagger.Workspace, modulePath string) ([]string, error) {
	goMods, err := goModPaths(ctx, ws)
	if err != nil {
		return nil, err
	}
	var excludes []string
	for _, goModPath := range goMods {
		nestedPath := modulePathForGoMod(goModPath)
		if modulePath == "." {
			if nestedPath != "." {
				excludes = append(excludes, nestedPath+"/**")
			}
			continue
		}
		if nestedPath != modulePath && strings.HasPrefix(nestedPath, strings.TrimSuffix(modulePath, "/")+"/") {
			excludes = append(excludes, nestedPath+"/**")
		}
	}
	sort.Strings(excludes)
	return excludes, nil
}

func goModReplaceIncludes(ctx context.Context, ws *dagger.Workspace, all *includeSet) (_ []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes scan go.mod replaces")
	defer telemetry.EndWithCause(span, &rerr)

	seenGoMods := map[string]bool{}
	includes := newIncludeSet()
	passCount := 0
	for {
		dir := workspaceDirectory(ws, all.list, nil)
		goMods, err := dir.Glob(ctx, "**/go.mod")
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
			data, err := readRegularFile(ctx, dir, goModPath)
			if err != nil {
				return nil, err
			}
			replaces, err := goModLocalReplaceIncludes(goModPath, data)
			if err != nil {
				return nil, err
			}
			all.add(replaces...)
			includes.add(replaces...)
		}
	}
	span.SetAttributes(
		attribute.Int("go_includes.go_mod_count", len(seenGoMods)),
		attribute.Int("go_includes.pass_count", passCount),
		attribute.Int("go_includes.include_count", len(includes.list)),
	)
	return includes.list, nil
}

func generateModules(ctx context.Context, ws *dagger.Workspace, onlyModule string) ([]string, error) {
	goMods, err := goModPaths(ctx, ws)
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
	dir := workspaceDirectory(ws, include, nil)
	results, err := dir.Search(ctx, "//go:generate", dagger.DirectorySearchOpts{
		Globs:     []string{"**/*.go"},
		Literal:   true,
		FilesOnly: true,
	})
	if err != nil {
		return nil, err
	}

	hasGenerate := map[string]bool{}
	for _, result := range results {
		goFile, err := result.FilePath(ctx)
		if err != nil {
			return nil, err
		}
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

func goModPaths(ctx context.Context, ws *dagger.Workspace) ([]string, error) {
	goMods, err := workspaceDirectory(ws, []string{"**/go.mod"}, nil).Glob(ctx, "**/go.mod")
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

func goDirectiveIncludesFromBytes(filePath string, data []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	prefix := includePrefixForGoFile(filePath)
	var includes []string
	for _, group := range file.Comments {
		for _, comment := range group.List {
			patterns, err := includePatternsFromComment(comment.Text)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", fset.Position(comment.Slash), err)
			}
			for _, pattern := range patterns {
				includes = append(includes, addIncludePrefix(prefix, pattern))
			}
		}
	}
	return includes, nil
}

func includePrefixForGoFile(filePath string) string {
	filePath = cleanWorkspacePath(filePath)
	fileDir := path.Dir(filePath)
	if fileDir == "." {
		return ""
	}
	return fileDir
}

func includePatternsFromComment(comment string) ([]string, error) {
	if strings.HasPrefix(comment, "//go:embed") {
		args := strings.TrimPrefix(comment, "//go:embed")
		if args != "" && !startsWithSpace(args) {
			return nil, nil
		}
		patterns, err := parseDirectiveArgs("go:embed", args)
		if err != nil {
			return nil, err
		}
		for i, pattern := range patterns {
			patterns[i] = strings.TrimPrefix(pattern, "all:")
		}
		return patterns, nil
	}

	if strings.HasPrefix(comment, "//go:generate") {
		args := strings.TrimPrefix(comment, "//go:generate")
		if args != "" && !startsWithSpace(args) {
			return nil, nil
		}
		parsed, err := parseDirectiveArgs("go:generate", args)
		if err != nil {
			return nil, err
		}
		return goGenerateIncludes(parsed), nil
	}

	text := strings.TrimSpace(strings.TrimPrefix(comment, "//"))
	if !strings.HasPrefix(text, "workspace:include") {
		return nil, nil
	}
	args := strings.TrimPrefix(text, "workspace:include")
	if args != "" && !startsWithSpace(args) {
		return nil, nil
	}
	return parseDirectiveArgs("workspace:include", args)
}

func goGenerateIncludes(args []string) []string {
	if len(args) == 0 || args[0] != "go" {
		return nil
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-C" {
			if i+1 >= len(args) {
				return nil
			}
			return goModuleSourceIncludes(args[i+1])
		}
		if dir, ok := strings.CutPrefix(arg, "-C="); ok {
			return goModuleSourceIncludes(dir)
		}
		if !strings.HasPrefix(arg, "-") {
			return nil
		}
	}
	return nil
}

func goModuleSourceIncludes(dir string) []string {
	patterns := []string{
		"**/*.go",
		"**/*.c",
		"**/*.h",
		"**/*.s",
		"**/*.S",
		"**/*.syso",
		"go.mod",
		"go.sum",
		"go.work",
		"go.work.sum",
	}
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" || dir == "." {
		return patterns
	}
	for i, pattern := range patterns {
		patterns[i] = dir + "/" + pattern
	}
	return patterns
}

func goModLocalReplaceIncludes(goModPath string, data []byte) ([]string, error) {
	file, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, err
	}

	var includes []string
	for _, replace := range file.Replace {
		if replace.New.Version != "" || !isWorkspaceRelativePath(replace.New.Path) {
			continue
		}
		target := strings.TrimSuffix(replace.New.Path, "/")
		includes = append(includes, addIncludePrefix(path.Dir(goModPath), target+"/**"))
	}
	return includes, nil
}

func parseDirectiveArgs(name, args string) ([]string, error) {
	var parsed []string
	for args = strings.TrimLeftFunc(args, unicode.IsSpace); args != ""; args = strings.TrimLeftFunc(args, unicode.IsSpace) {
		switch args[0] {
		case '`', '"':
			quoted, err := strconv.QuotedPrefix(args)
			if err != nil {
				return nil, fmt.Errorf("invalid quoted string in //%s: %s", name, args)
			}
			arg, err := strconv.Unquote(quoted)
			if err != nil {
				return nil, fmt.Errorf("invalid quoted string in //%s: %s", name, quoted)
			}
			parsed = append(parsed, arg)
			args = args[len(quoted):]
			if args != "" && !startsWithSpace(args) {
				return nil, fmt.Errorf("invalid quoted string in //%s: %s", name, args)
			}
		default:
			i := strings.IndexFunc(args, unicode.IsSpace)
			if i < 0 {
				i = len(args)
			}
			parsed = append(parsed, args[:i])
			args = args[i:]
		}
	}
	return parsed, nil
}

func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
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

func addIncludePrefix(prefix, pattern string) string {
	if strings.HasPrefix(pattern, "/") {
		return strings.TrimPrefix(pattern, "/")
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" || prefix == "." {
		return pattern
	}
	return prefix + "/" + pattern
}

type includeSet struct {
	list []string
	seen map[string]struct{}
}

func newIncludeSet(patterns ...string) *includeSet {
	set := &includeSet{}
	set.add(patterns...)
	return set
}

func (s *includeSet) add(patterns ...string) {
	if s.seen == nil {
		s.seen = map[string]struct{}{}
	}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if _, ok := s.seen[pattern]; ok {
			continue
		}
		s.seen[pattern] = struct{}{}
		s.list = append(s.list, pattern)
	}
}
