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

func usage() {
	fmt.Fprintln(os.Stderr, "usage: go-includes [--lint] [--test] [--generate] [DIR]")
}

func run(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags()
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
	ws, err := currentWorkspace(ctx)
	if err != nil {
		return nil, err
	}

	modulePath := "."
	if flags.NArg() == 1 {
		modulePath = flags.Arg(0)
	}
	modulePathClean := cleanWorkspacePath(modulePath)
	includes, rerr = moduleIncludes(ctx, ws, modulePathClean, mode)
	span.SetAttributes(
		attribute.String("go_includes.module_path", modulePathClean),
		attribute.String("go_includes.mode", mode.String()),
		attribute.Int("go_includes.include_count", len(includes)),
	)
	return includes, rerr
}

func newFlags() *flag.FlagSet {
	flags := flag.NewFlagSet("go-includes", flag.ExitOnError)
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

func moduleIncludes(ctx context.Context, ws *dagger.Workspace, modulePath string, mode includeModes) ([]string, error) {
	walker, err := newIncludeWalker(ctx, ws, modulePath, mode)
	if err != nil {
		return nil, err
	}
	return walker.walk(ctx)
}

type includeWalker struct {
	ws          *dagger.Workspace
	mode        includeModes
	initial     string
	modulePaths []string
	moduleSet   map[string]bool
	queued      map[string]bool
	queue       []string
	includes    *includeSet
}

func newIncludeWalker(ctx context.Context, ws *dagger.Workspace, initial string, mode includeModes) (*includeWalker, error) {
	goMods, err := goModPaths(ctx, ws)
	if err != nil {
		return nil, err
	}
	modulePaths := make([]string, 0, len(goMods))
	moduleSet := map[string]bool{}
	for _, goModPath := range goMods {
		modulePath := modulePathForGoMod(goModPath)
		modulePaths = append(modulePaths, modulePath)
		moduleSet[modulePath] = true
	}

	initialClean := cleanWorkspacePath(initial)
	initialModule, ok := containingModuleDir(initialClean, moduleSet)
	if !ok {
		return nil, fmt.Errorf("no go.mod found containing path: %s", initialClean)
	}

	walker := &includeWalker{
		ws:          ws,
		mode:        mode,
		initial:     initialModule,
		modulePaths: modulePaths,
		moduleSet:   moduleSet,
		queued:      map[string]bool{},
		includes:    newIncludeSet(),
	}
	if err := walker.enqueue(walker.initial); err != nil {
		return nil, err
	}
	return walker, nil
}

func (w *includeWalker) enqueue(modulePath string) error {
	modulePath = cleanWorkspacePath(modulePath)
	if escapesWorkspace(modulePath) {
		return fmt.Errorf("module path escapes workspace: %s", modulePath)
	}
	if !w.moduleSet[modulePath] {
		return fmt.Errorf("no go.mod found for module root: %s", modulePath)
	}
	if w.queued[modulePath] {
		return nil
	}
	w.queued[modulePath] = true
	w.queue = append(w.queue, modulePath)
	return nil
}

func (w *includeWalker) walk(ctx context.Context) ([]string, error) {
	for len(w.queue) > 0 {
		modulePath := w.queue[0]
		w.queue = w.queue[1:]

		if modulePath != w.initial {
			w.includes.add(baseIncludes(modulePath)...)
		}

		scan, err := directiveIncludes(ctx, w.ws, modulePath, w.mode, w.modulePaths, w.moduleSet)
		if err != nil {
			return nil, err
		}
		w.includes.add(scan.includes...)

		replaces, err := goModLocalReplaceModules(ctx, w.ws, modulePath)
		if err != nil {
			return nil, err
		}
		for _, next := range append(replaces, scan.modules...) {
			if err := w.enqueue(next); err != nil {
				return nil, err
			}
		}
	}
	return w.includes.list, nil
}

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

type directiveScan struct {
	includes []string
	modules  []string
}

func directiveIncludes(ctx context.Context, ws *dagger.Workspace, modulePath string, mode includeModes, modulePaths []string, moduleSet map[string]bool) (_ directiveScan, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes scan directives")
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

func includePrefixForGoFile(filePath string) string {
	filePath = cleanWorkspacePath(filePath)
	fileDir := path.Dir(filePath)
	if fileDir == "." {
		return ""
	}
	return fileDir
}

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

func goModLocalReplaceModules(ctx context.Context, ws *dagger.Workspace, modulePath string) ([]string, error) {
	goModPath := addIncludePrefix(modulePath, "go.mod")
	dir := workspaceDirectory(ws, []string{goModPath}, nil)
	data, err := readRegularFile(ctx, dir, goModPath)
	if err != nil {
		return nil, err
	}
	return goModLocalReplaceModulePaths(goModPath, data)
}

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
