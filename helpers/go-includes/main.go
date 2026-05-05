// go.dang runs this helper to discover workspace include patterns.
// It emits one include pattern per line.
package main

import (
	"context"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	telemetry "github.com/dagger/otel-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

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
	defer func() { _ = closeDaggerClient() }()

	var (
		includes []string
		err      error
	)
	switch os.Args[1] {
	case "module":
		includes, err = runModule(ctx, os.Args[2:])
	case "generate-modules":
		includes, err = runGenerateModules(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, include := range includes {
		fmt.Println(include)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  go-includes module --path=DIR [--no-recursive]")
	fmt.Fprintln(os.Stderr, "  go-includes generate-modules [--path=DIR]")
}

func runModule(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes module")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags("module")
	modulePath := flags.String("path", "", "workspace-relative Go module root")
	noRecursive := flags.Bool("no-recursive", false, "only scan go.mod files discovered before local replace includes")
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
	discovery := moduleDiscovery{
		workspace:  daggerWorkspace{ws: ws},
		modulePath: cleanWorkspacePath(*modulePath),
		recursive:  !*noRecursive,
	}
	includes, rerr = discovery.includes(ctx)
	span.SetAttributes(
		attribute.String("go_includes.module_path", discovery.modulePath),
		attribute.Bool("go_includes.recursive", discovery.recursive),
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
	modules, rerr = generateModules(ctx, daggerWorkspace{ws: ws}, onlyModule)
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

func goDirectiveIncludesFromBytes(filePath, prefix string, data []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

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
