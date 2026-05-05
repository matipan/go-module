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
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"dagger.io/dagger"
	telemetry "github.com/dagger/otel-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/mod/modfile"
)

var errNotRegularFile = errors.New("not a regular file")

const instrumentationLibrary = "go-includes"

func main() {
	ctx := telemetry.Init(context.Background(), telemetry.Config{Detect: true})
	defer telemetry.Close()

	target, err := newTarget(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := target.print(ctx, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newTarget parses CLI flags and resolves the requested workspace module.
func newTarget(ctx context.Context, cliArgs []string) (*target, error) {
	flags := flag.NewFlagSet("go-includes", flag.ExitOnError)
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go-includes [--lint] [--test] [--generate] [/DIR]")
		flags.PrintDefaults()
	}
	lint := flags.Bool("lint", false, "include lint inputs")
	test := flags.Bool("test", false, "include test inputs")
	generate := flags.Bool("generate", false, "include generate inputs")
	if err := flags.Parse(cliArgs); err != nil {
		return nil, err
	}
	if flags.NArg() > 1 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if !*lint && !*test && !*generate {
		*test = true
	}
	modulePath := "/"
	if flags.NArg() == 1 {
		modulePath = flags.Arg(0)
		if !strings.HasPrefix(modulePath, "/") {
			return nil, fmt.Errorf("workspace path must be absolute: %s", modulePath)
		}
	}
	modulePathClean := cleanWorkspacePath(modulePath)
	ws, err := newWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	moduleRoot, ok := ws.containingModuleDir(modulePathClean)
	if !ok {
		return nil, fmt.Errorf("no go.mod found containing path: %s", modulePathClean)
	}
	return &target{
		workspace:  ws,
		moduleRoot: moduleRoot,
		lint:       *lint,
		test:       *test,
		generate:   *generate,
	}, nil
}

// Tracer returns the helper's telemetry tracer.
func Tracer(ctx context.Context) trace.Tracer {
	return telemetry.Tracer(ctx, instrumentationLibrary)
}

// modeString returns a compact operation label for trace attributes.
func (t target) modeString() string {
	var names []string
	if t.lint {
		names = append(names, "lint")
	}
	if t.test {
		names = append(names, "test")
	}
	if t.generate {
		names = append(names, "generate")
	}
	if len(names) == 0 {
		return "test"
	}
	return strings.Join(names, ",")
}

// workspace wraps a Dagger workspace with indexes shared by include targets.
type workspace struct {
	*dagger.Workspace

	moduleRoots []string
	moduleSet   map[string]bool
}

// newWorkspace loads the current Dagger workspace and indexes its Go modules.
func newWorkspace(ctx context.Context) (*workspace, error) {
	dagWS, err := currentWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	ws := &workspace{Workspace: dagWS}
	if err := ws.indexModules(ctx); err != nil {
		return nil, err
	}
	return ws, nil
}

// directory returns a workspace-root directory filtered by include globs.
func (w *workspace) directory(include, exclude []string) *dagger.Directory {
	return w.Directory("/", dagger.WorkspaceDirectoryOpts{
		Include: append([]string(nil), include...),
		Exclude: append([]string(nil), exclude...),
	})
}

// indexModules records every module root, plus a set for ancestor lookups.
func (w *workspace) indexModules(ctx context.Context) error {
	goMods, err := w.directory([]string{"**/go.mod"}, nil).Glob(ctx, "**/go.mod")
	if err != nil {
		return err
	}
	sort.Strings(goMods)
	w.moduleRoots = make([]string, 0, len(goMods))
	w.moduleSet = map[string]bool{}
	for _, goModPath := range goMods {
		moduleRoot := strings.TrimSuffix(goModPath, "/go.mod")
		if goModPath == "go.mod" {
			moduleRoot = "."
		}
		w.moduleRoots = append(w.moduleRoots, moduleRoot)
		w.moduleSet[moduleRoot] = true
	}
	return nil
}

// containingModuleDir finds the nearest ancestor module root for a workspace path.
func (w *workspace) containingModuleDir(dir string) (string, bool) {
	dir = cleanWorkspacePath(dir)
	for {
		if w.moduleSet[dir] {
			return dir, true
		}
		if dir == "." {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// nestedModuleExcludes returns globs that keep a scan within one module.
func (w *workspace) nestedModuleExcludes(moduleRoot string) []string {
	var excludes []string
	for _, nestedRoot := range w.moduleRoots {
		if moduleRoot == "." {
			if nestedRoot != "." {
				excludes = append(excludes, nestedRoot+"/**")
			}
			continue
		}
		if nestedRoot != moduleRoot && strings.HasPrefix(nestedRoot, strings.TrimSuffix(moduleRoot, "/")+"/") {
			excludes = append(excludes, nestedRoot+"/**")
		}
	}
	sort.Strings(excludes)
	return excludes
}

// localReplaceModules returns local replace targets for one module's go.mod.
func (w *workspace) localReplaceModules(ctx context.Context, moduleRoot string) ([]string, error) {
	goModPath := addIncludePrefix(moduleRoot, "go.mod")
	dir := w.directory([]string{goModPath}, nil)
	data, err := readRegularFile(ctx, dir, goModPath)
	if err != nil {
		return nil, err
	}
	return goModLocalReplaceModulePaths(goModPath, data)
}

// target is the requested module root and operation-specific include behavior.
type target struct {
	workspace  *workspace
	moduleRoot string
	lint       bool
	test       bool
	generate   bool
}

// includes traverses module roots discovered from replaces and generate workdirs.
func (t target) includes(ctx context.Context) ([]string, error) {
	initialModule := t.moduleRoot

	queued := map[string]bool{initialModule: true}
	queue := []string{initialModule}
	var includes []string
	enqueue := func(modulePath string) error {
		modulePath = cleanWorkspacePath(modulePath)
		if escapesWorkspace(modulePath) {
			return fmt.Errorf("module path escapes workspace: %s", modulePath)
		}
		if !t.workspace.moduleSet[modulePath] {
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
			includes = append(includes, baseIncludes(modulePath)...)
		}

		scan, err := t.scanModuleDirectives(ctx, modulePath)
		if err != nil {
			return nil, err
		}
		includes = append(includes, scan.includes...)

		replaces, err := t.workspace.localReplaceModules(ctx, modulePath)
		if err != nil {
			return nil, err
		}
		for _, next := range append(replaces, scan.modules...) {
			if err := enqueue(next); err != nil {
				return nil, err
			}
		}
	}
	deduped := make([]string, 0, len(includes))
	seen := map[string]bool{}
	for _, include := range includes {
		if seen[include] {
			continue
		}
		seen[include] = true
		deduped = append(deduped, include)
	}
	return deduped, nil
}

// print writes the target include patterns, one per line.
func (t target) print(ctx context.Context, w io.Writer) error {
	includes, err := t.includes(ctx)
	if err != nil {
		return err
	}
	for _, include := range includes {
		if _, err := fmt.Fprintln(w, include); err != nil {
			return err
		}
	}
	return nil
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

// discoveredInputs records include patterns and module roots found in directives.
type discoveredInputs struct {
	includes []string
	modules  []string
}

// scanModuleDirectives scans Go directives in one module.
func (t target) scanModuleDirectives(ctx context.Context, modulePath string) (_ discoveredInputs, rerr error) {
	ctx, span := Tracer(ctx).Start(ctx, "go-includes scan directives")
	defer telemetry.EndWithCause(span, &rerr)

	directives, goFileCount, err := t.goDirectives(ctx, modulePath)
	if err != nil {
		return discoveredInputs{}, err
	}
	scan, err := t.includesFromGoDirectives(directives)
	if err != nil {
		return discoveredInputs{}, err
	}
	span.SetAttributes(
		attribute.String("go_includes.mode", t.modeString()),
		attribute.String("go_includes.module_path", modulePath),
		attribute.Int("go_includes.file_count", goFileCount),
		attribute.Int("go_includes.include_count", len(scan.includes)),
		attribute.Int("go_includes.discovered_module_count", len(scan.modules)),
	)
	return scan, nil
}

// goDirectives returns parsed Go comment directives for one module.
func (t target) goDirectives(ctx context.Context, modulePath string) ([]goDirective, int, error) {
	excludes := t.workspace.nestedModuleExcludes(modulePath)
	dir := t.workspace.directory([]string{addIncludePrefix(modulePath, "**/*.go")}, excludes)
	goFiles, err := dir.Glob(ctx, "**/*.go")
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(goFiles)

	var directives []goDirective
	for _, filePath := range goFiles {
		data, err := readRegularFile(ctx, dir, filePath)
		if errors.Is(err, errNotRegularFile) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		fileDirectives, err := goDirectivesInFile(filePath, data)
		if err != nil {
			return nil, 0, err
		}
		directives = append(directives, fileDirectives...)
	}
	return directives, len(goFiles), nil
}

// includesFromGoDirectives resolves directive inputs for the target mode.
func (t target) includesFromGoDirectives(directives []goDirective) (discoveredInputs, error) {
	var scan discoveredInputs
	for _, directive := range directives {
		includes, err := directive.includes(t.test, t.lint, t.generate)
		if err != nil {
			return discoveredInputs{}, err
		}
		scan.includes = append(scan.includes, includes...)
		if !t.generate || !directive.isGenerate() {
			continue
		}
		if !directive.isGenerateGoDashC() {
			continue
		}
		workdir, err := directive.generateGoDashCValue()
		if err != nil {
			return discoveredInputs{}, err
		}
		workdir = cleanWorkspacePath(addIncludePrefix(directive.dir(), workdir))
		if t.workspace == nil {
			scan.modules = append(scan.modules, workdir)
			continue
		}
		module, ok := t.workspace.containingModuleDir(workdir)
		if !ok {
			return discoveredInputs{}, fmt.Errorf("%s: no Go module found for go -C directory: %s", directive.position, workdir)
		}
		scan.modules = append(scan.modules, module)
	}
	return scan, nil
}

// scanGoFileDirectives parses one Go file and resolves directive paths.
func scanGoFileDirectives(filePath string, data []byte, test, generate bool) (discoveredInputs, error) {
	directives, err := goDirectivesInFile(filePath, data)
	if err != nil {
		return discoveredInputs{}, err
	}
	return (target{test: test, generate: generate}).includesFromGoDirectives(directives)
}

// goDirectivesInFile extracts Go comment directives from one parsed Go file.
func goDirectivesInFile(filePath string, data []byte) ([]goDirective, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	filePath = cleanWorkspacePath(filePath)
	var directives []goDirective
	for _, group := range file.Comments {
		for _, comment := range group.List {
			directive := goDirective{
				filePath: filePath,
				position: fset.Position(comment.Slash).String(),
				comment:  comment.Text,
			}
			if directive.isKnown() {
				directives = append(directives, directive)
			}
		}
	}
	return directives, nil
}

// goDirective is one supported Go line directive comment.
type goDirective struct {
	filePath string
	position string
	comment  string
}

// dir returns the directive's workspace directory.
func (d goDirective) dir() string {
	dir := path.Dir(cleanWorkspacePath(d.filePath))
	if dir == "." {
		return ""
	}
	return dir
}

// isKnown reports whether the directive is relevant to include discovery.
func (d goDirective) isKnown() bool {
	return d.isEmbed() || d.isTestInclude() || d.isGenerateInclude() || d.isGenerate()
}

// isEmbed reports whether the directive is //go:embed.
func (d goDirective) isEmbed() bool {
	return d.hasName("go:embed")
}

// isTestInclude reports whether the directive is //go:test:include.
func (d goDirective) isTestInclude() bool {
	return d.hasName("go:test:include")
}

// isGenerateInclude reports whether the directive is //go:generate:include.
func (d goDirective) isGenerateInclude() bool {
	return d.hasName("go:generate:include")
}

// isGenerate reports whether the directive is //go:generate.
func (d goDirective) isGenerate() bool {
	return d.hasName("go:generate")
}

// isGenerateGoDashC reports whether the directive is a go command with -C.
func (d goDirective) isGenerateGoDashC() bool {
	_, ok, _ := d.generateGoDashC()
	return ok
}

// args parses the directive arguments.
func (d goDirective) args() ([]string, error) {
	name, argString, ok := d.nameAndArgs()
	if !ok {
		return nil, nil
	}
	args, err := parseDirectiveArgs(name, argString)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.position, err)
	}
	return args, nil
}

// generateGoDashCValue returns the -C value for a generate directive, if present.
func (d goDirective) generateGoDashCValue() (string, error) {
	workdir, _, err := d.generateGoDashC()
	return workdir, err
}

// includes returns include patterns enabled by the target modes.
func (d goDirective) includes(test, lint, generate bool) ([]string, error) {
	if d.isEmbed() {
		patterns, err := d.args()
		if err != nil {
			return nil, err
		}
		for i, pattern := range patterns {
			patterns[i] = strings.TrimPrefix(pattern, "all:")
		}
		return d.prefixed(patterns), nil
	}
	if test && d.isTestInclude() {
		patterns, err := d.args()
		if err != nil {
			return nil, err
		}
		return d.prefixed(patterns), nil
	}
	if generate && d.isGenerateInclude() {
		patterns, err := d.args()
		if err != nil {
			return nil, err
		}
		return d.prefixed(patterns), nil
	}
	return nil, nil
}

// prefixed resolves directive patterns relative to the directive's file.
func (d goDirective) prefixed(patterns []string) []string {
	for i, pattern := range patterns {
		patterns[i] = addIncludePrefix(d.dir(), pattern)
	}
	return patterns
}

// hasName reports whether the directive has the exact directive name.
func (d goDirective) hasName(name string) bool {
	_, ok := d.argsForName(name)
	return ok
}

// nameAndArgs returns the directive name and raw argument tail.
func (d goDirective) nameAndArgs() (string, string, bool) {
	for _, name := range []string{"go:embed", "go:test:include", "go:generate:include", "go:generate"} {
		if args, ok := d.argsForName(name); ok {
			return name, args, true
		}
	}
	return "", "", false
}

// argsForName returns the argument tail for an exact line directive name.
func (d goDirective) argsForName(name string) (string, bool) {
	prefix := "//" + name
	if !strings.HasPrefix(d.comment, prefix) {
		return "", false
	}
	args := strings.TrimPrefix(d.comment, prefix)
	if args != "" && strings.TrimLeftFunc(args, unicode.IsSpace) == args {
		return "", false
	}
	return args, true
}

// generateGoDashC recognizes go generate commands that change directory with -C.
func (d goDirective) generateGoDashC() (string, bool, error) {
	if !d.isGenerate() {
		return "", false, nil
	}
	args, err := d.args()
	if err != nil {
		return "", false, err
	}
	if len(args) == 0 || args[0] != "go" {
		return "", false, nil
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-C" {
			if i+1 >= len(args) {
				return "", false, nil
			}
			return args[i+1], true, nil
		}
		if dir, ok := strings.CutPrefix(arg, "-C="); ok {
			return dir, true, nil
		}
		if !strings.HasPrefix(arg, "-") {
			return "", false, nil
		}
	}
	return "", false, nil
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
			if args != "" && strings.TrimLeftFunc(args, unicode.IsSpace) == args {
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
