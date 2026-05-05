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
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/mod/modfile"
)

var errNotRegularFile = errors.New("not a regular file")

// includeModes selects the operation-specific directive families to honor.
type includeModes struct {
	lint     bool
	test     bool
	generate bool
}

var (
	modeLint     = includeModes{lint: true}
	modeTest     = includeModes{test: true}
	modeGenerate = includeModes{generate: true}
)

// includeModesFromFlags converts CLI mode flags into scan behavior.
func includeModesFromFlags(lint, test, generate bool) includeModes {
	if !lint && !test && !generate {
		test = true
	}
	return includeModes{
		lint:     lint,
		test:     test,
		generate: generate,
	}
}

// String returns a compact mode label for trace attributes.
func (m includeModes) String() string {
	var names []string
	if m.lint {
		names = append(names, "lint")
	}
	if m.test {
		names = append(names, "test")
	}
	if m.generate {
		names = append(names, "generate")
	}
	if len(names) == 0 {
		return "test"
	}
	return strings.Join(names, ",")
}

func main() {
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
	lines, err = run(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, line := range lines {
		fmt.Println(line)
	}
}

// run parses CLI flags and emits include patterns for an absolute workspace path.
func run(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := telemetry.Tracer(ctx, "go-includes").Start(ctx, "go-includes")
	defer telemetry.EndWithCause(span, &rerr)

	flags := flag.NewFlagSet("go-includes", flag.ExitOnError)
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go-includes [--lint] [--test] [--generate] [/DIR]")
		flags.PrintDefaults()
	}
	lint := flags.Bool("lint", false, "include lint inputs")
	test := flags.Bool("test", false, "include test inputs")
	generate := flags.Bool("generate", false, "include generate inputs")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 1 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	mode := includeModesFromFlags(*lint, *test, *generate)
	modulePath := "/"
	if flags.NArg() == 1 {
		modulePath = flags.Arg(0)
		if !strings.HasPrefix(modulePath, "/") {
			return nil, fmt.Errorf("workspace path must be absolute: %s", modulePath)
		}
	}
	modulePathClean := cleanWorkspacePath(modulePath)
	ws, err := currentWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	includes, rerr = walkIncludes(ctx, ws, modulePathClean, mode)
	span.SetAttributes(
		attribute.String("go_includes.module_path", modulePathClean),
		attribute.String("go_includes.mode", mode.String()),
		attribute.Int("go_includes.include_count", len(includes)),
	)
	return includes, rerr
}

// workspaceDirectory returns a workspace-root directory filtered by include globs.
func workspaceDirectory(ws *dagger.Workspace, include, exclude []string) *dagger.Directory {
	return ws.Directory("/", dagger.WorkspaceDirectoryOpts{
		Include: append([]string(nil), include...),
		Exclude: append([]string(nil), exclude...),
	})
}

// readRegularFile reads a file from a Dagger directory and reports directories distinctly.
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

// walkIncludes traverses module roots discovered from replaces and generate workdirs.
func walkIncludes(ctx context.Context, ws *dagger.Workspace, initial string, mode includeModes) ([]string, error) {
	modulePaths, moduleSet, err := moduleIndex(ctx, ws)
	if err != nil {
		return nil, err
	}

	initialClean := cleanWorkspacePath(initial)
	initialModule, ok := containingModuleDir(initialClean, moduleSet)
	if !ok {
		return nil, fmt.Errorf("no go.mod found containing path: %s", initialClean)
	}

	queued := map[string]bool{initialModule: true}
	queue := []string{initialModule}
	includes := newIncludeSet()
	enqueue := func(modulePath string) error {
		modulePath = cleanWorkspacePath(modulePath)
		if escapesWorkspace(modulePath) {
			return fmt.Errorf("module path escapes workspace: %s", modulePath)
		}
		if !moduleSet[modulePath] {
			return fmt.Errorf("no go.mod found for module root: %s", modulePath)
		}
		if !queued[modulePath] {
			queued[modulePath] = true
			queue = append(queue, modulePath)
		}
		return nil
	}

	for len(queue) > 0 {
		modulePath := queue[0]
		queue = queue[1:]

		if modulePath != initialModule {
			includes.add(baseIncludes(modulePath)...)
		}

		scan, err := directiveIncludes(ctx, ws, modulePath, mode, modulePaths, moduleSet)
		if err != nil {
			return nil, err
		}
		includes.add(scan.includes...)

		replaces, err := goModLocalReplaceModules(ctx, ws, modulePath)
		if err != nil {
			return nil, err
		}
		for _, next := range append(replaces, scan.modules...) {
			if err := enqueue(next); err != nil {
				return nil, err
			}
		}
	}
	return includes.list, nil
}

// baseIncludes returns the static Go source patterns for a module root.
func baseIncludes(modulePath string) []string {
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
	for i, pattern := range patterns {
		patterns[i] = addIncludePrefix(modulePath, pattern)
	}
	return patterns
}

// directiveScan records include patterns and additional module roots from comments.
type directiveScan struct {
	includes []string
	modules  []string
}

// directiveIncludes scans Go files in one module, excluding nested modules.
func directiveIncludes(ctx context.Context, ws *dagger.Workspace, modulePath string, mode includeModes, modulePaths []string, moduleSet map[string]bool) (_ directiveScan, rerr error) {
	ctx, span := telemetry.Tracer(ctx, "go-includes").Start(ctx, "go-includes scan directives")
	defer telemetry.EndWithCause(span, &rerr)

	excludes := nestedModuleExcludes(modulePaths, modulePath)
	dir := workspaceDirectory(ws, []string{addIncludePrefix(modulePath, "**/*.go")}, excludes)
	goFiles, err := dir.Glob(ctx, "**/*.go")
	if err != nil {
		return directiveScan{}, err
	}
	sort.Strings(goFiles)

	includes := newIncludeSet()
	modules := newIncludeSet()
	for _, filePath := range goFiles {
		data, err := readRegularFile(ctx, dir, filePath)
		if errors.Is(err, errNotRegularFile) {
			continue
		}
		if err != nil {
			return directiveScan{}, err
		}
		fileScan, err := goDirectiveIncludesFromBytes(filePath, data, mode)
		if err != nil {
			return directiveScan{}, err
		}
		includes.add(fileScan.includes...)
		for _, workdir := range fileScan.modules {
			module, ok := containingModuleDir(workdir, moduleSet)
			if !ok {
				return directiveScan{}, fmt.Errorf("%s: no Go module found for go -C directory: %s", filePath, workdir)
			}
			modules.add(module)
		}
	}
	span.SetAttributes(
		attribute.String("go_includes.mode", mode.String()),
		attribute.String("go_includes.module_path", modulePath),
		attribute.Int("go_includes.file_count", len(goFiles)),
		attribute.Int("go_includes.include_count", len(includes.list)),
		attribute.Int("go_includes.discovered_module_count", len(modules.list)),
	)
	return directiveScan{includes: includes.list, modules: modules.list}, nil
}

// nestedModuleExcludes returns globs that keep a scan within one module.
func nestedModuleExcludes(modulePaths []string, modulePath string) []string {
	var excludes []string
	for _, nestedPath := range modulePaths {
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
	return excludes
}

// moduleIndex returns every module root, plus a set for ancestor lookups.
func moduleIndex(ctx context.Context, ws *dagger.Workspace) ([]string, map[string]bool, error) {
	goMods, err := workspaceDirectory(ws, []string{"**/go.mod"}, nil).Glob(ctx, "**/go.mod")
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(goMods)
	modulePaths := make([]string, 0, len(goMods))
	moduleSet := map[string]bool{}
	for _, goModPath := range goMods {
		modulePath := strings.TrimSuffix(goModPath, "/go.mod")
		if goModPath == "go.mod" {
			modulePath = "."
		}
		modulePaths = append(modulePaths, modulePath)
		moduleSet[modulePath] = true
	}
	return modulePaths, moduleSet, nil
}

// containingModuleDir finds the nearest ancestor module root for a workspace path.
func containingModuleDir(dir string, moduleSet map[string]bool) (string, bool) {
	dir = cleanWorkspacePath(dir)
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

// goDirectiveIncludesFromBytes parses one Go file and resolves directive paths.
func goDirectiveIncludesFromBytes(filePath string, data []byte, mode includeModes) (directiveScan, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if err != nil {
		return directiveScan{}, err
	}

	prefix := includePrefixForGoFile(filePath)
	scan := directiveScan{}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			commentScan, err := includePatternsFromComment(comment.Text, mode)
			if err != nil {
				return directiveScan{}, fmt.Errorf("%s: %w", fset.Position(comment.Slash), err)
			}
			for _, pattern := range commentScan.includes {
				scan.includes = append(scan.includes, addIncludePrefix(prefix, pattern))
			}
			for _, workdir := range commentScan.modules {
				scan.modules = append(scan.modules, cleanWorkspacePath(addIncludePrefix(prefix, workdir)))
			}
		}
	}
	return scan, nil
}

// includePrefixForGoFile returns the workspace directory containing a Go file.
func includePrefixForGoFile(filePath string) string {
	filePath = cleanWorkspacePath(filePath)
	fileDir := path.Dir(filePath)
	if fileDir == "." {
		return ""
	}
	return fileDir
}

// includePatternsFromComment extracts supported include directives from one comment.
func includePatternsFromComment(comment string, mode includeModes) (directiveScan, error) {
	if args, ok := directiveArgs(comment, "go:embed"); ok {
		patterns, err := parseDirectiveArgs("go:embed", args)
		if err != nil {
			return directiveScan{}, err
		}
		for i, pattern := range patterns {
			patterns[i] = strings.TrimPrefix(pattern, "all:")
		}
		return directiveScan{includes: patterns}, nil
	}

	if mode.test {
		if args, ok := directiveArgs(comment, "go:test:include"); ok {
			patterns, err := parseDirectiveArgs("go:test:include", args)
			if err != nil {
				return directiveScan{}, err
			}
			return directiveScan{includes: patterns}, nil
		}
	}

	if mode.generate {
		if args, ok := directiveArgs(comment, "go:generate:include"); ok {
			patterns, err := parseDirectiveArgs("go:generate:include", args)
			if err != nil {
				return directiveScan{}, err
			}
			return directiveScan{includes: patterns}, nil
		}

		args, ok := directiveArgs(comment, "go:generate")
		if !ok {
			return directiveScan{}, nil
		}
		parsed, err := parseDirectiveArgs("go:generate", args)
		if err != nil {
			return directiveScan{}, err
		}
		if workdir, ok := goGenerateWorkdir(parsed); ok {
			return directiveScan{modules: []string{workdir}}, nil
		}
	}

	return directiveScan{}, nil
}

// directiveArgs returns the argument tail for an exact line directive name.
func directiveArgs(comment, name string) (string, bool) {
	prefix := "//" + name
	if !strings.HasPrefix(comment, prefix) {
		return "", false
	}
	args := strings.TrimPrefix(comment, prefix)
	if args != "" && !startsWithSpace(args) {
		return "", false
	}
	return args, true
}

// goGenerateWorkdir recognizes go generate commands that change directory with -C.
func goGenerateWorkdir(args []string) (string, bool) {
	if len(args) == 0 || args[0] != "go" {
		return "", false
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-C" {
			if i+1 >= len(args) {
				return "", false
			}
			return args[i+1], true
		}
		if dir, ok := strings.CutPrefix(arg, "-C="); ok {
			return dir, true
		}
		if !strings.HasPrefix(arg, "-") {
			return "", false
		}
	}
	return "", false
}

// goModLocalReplaceModules returns local replace targets for one module's go.mod.
func goModLocalReplaceModules(ctx context.Context, ws *dagger.Workspace, modulePath string) ([]string, error) {
	goModPath := addIncludePrefix(modulePath, "go.mod")
	dir := workspaceDirectory(ws, []string{goModPath}, nil)
	data, err := readRegularFile(ctx, dir, goModPath)
	if err != nil {
		return nil, err
	}
	return goModLocalReplaceModulePaths(goModPath, data)
}

// goModLocalReplaceModulePaths parses local replace directives into module roots.
func goModLocalReplaceModulePaths(goModPath string, data []byte) ([]string, error) {
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
		target = cleanWorkspacePath(addIncludePrefix(path.Dir(goModPath), target))
		if escapesWorkspace(target) {
			return nil, fmt.Errorf("local replace target escapes workspace: %s", replace.New.Path)
		}
		includes = append(includes, target)
	}
	return includes, nil
}

// parseDirectiveArgs splits directive arguments with Go string literal support.
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

// startsWithSpace reports whether a string begins with Unicode whitespace.
func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
}

// isWorkspaceRelativePath reports whether a go.mod path can point inside the workspace.
func isWorkspaceRelativePath(path string) bool {
	return strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../")
}

// cleanWorkspacePath converts an absolute workspace path into the internal relative form.
func cleanWorkspacePath(filePath string) string {
	return path.Clean(strings.TrimPrefix(filePath, "/"))
}

// escapesWorkspace reports whether a cleaned relative path leaves the workspace root.
func escapesWorkspace(filePath string) bool {
	return filePath == ".." || strings.HasPrefix(filePath, "../")
}

// addIncludePrefix resolves a directive pattern relative to a workspace directory.
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

// includeSet preserves insertion order while de-duplicating include patterns.
type includeSet struct {
	list []string
	seen map[string]struct{}
}

// newIncludeSet builds an ordered set preloaded with patterns.
func newIncludeSet(patterns ...string) *includeSet {
	set := &includeSet{}
	set.add(patterns...)
	return set
}

// add appends unseen, non-empty patterns to the set.
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
