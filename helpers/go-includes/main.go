// go.dang runs this helper to discover workspace include patterns.
// It emits one include pattern per line.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
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
	case "from-go-directives":
		includes, err = runFromGoDirectives(ctx, os.Args[2:])
	case "from-go-mod-replace":
		includes, err = runFromGoModReplace(ctx, os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "  go-includes from-go-directives [--add-prefix=DIR] [-- FILE.go [FILE.go...]]")
	fmt.Fprintln(os.Stderr, "  go-includes from-go-mod-replace [--no-recursive] [-- go.mod [go.mod...]]")
	fmt.Fprintln(os.Stderr, "  The module command reads seed include patterns from stdin, one per line.")
	fmt.Fprintln(os.Stderr, "  The other commands read paths from stdin when no paths are passed.")
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
	seedIncludes, err := inputLines(flags.Args(), os.Stdin)
	if err != nil {
		return nil, err
	}
	ws, err := currentWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	discovery := moduleDiscovery{
		workspace:    daggerWorkspace{ws: ws},
		modulePath:   cleanWorkspacePath(*modulePath),
		seedIncludes: seedIncludes,
		recursive:    !*noRecursive,
	}
	includes, rerr = discovery.includes(ctx)
	span.SetAttributes(
		attribute.String("go_includes.module_path", discovery.modulePath),
		attribute.Int("go_includes.seed_include_count", len(seedIncludes)),
		attribute.Bool("go_includes.recursive", discovery.recursive),
		attribute.Int("go_includes.include_count", len(includes)),
	)
	return includes, rerr
}

func runFromGoDirectives(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes from-go-directives")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags("from-go-directives")
	prefix := flags.String("add-prefix", "", "prefix to add to relative include patterns")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	paths, err := inputLines(flags.Args(), os.Stdin)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.Int("go_includes.file_count", len(paths)))
	for _, filePath := range paths {
		includePrefix := *prefix
		if includePrefix == "" {
			includePrefix = includePrefixForGoFile(filePath)
		}
		fileIncludes, err := goDirectiveIncludes(filePath, includePrefix)
		if err != nil {
			return nil, err
		}
		includes = append(includes, fileIncludes...)
	}
	span.SetAttributes(attribute.Int("go_includes.include_count", len(includes)))
	return includes, rerr
}

func runFromGoModReplace(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes from-go-mod-replace")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags("from-go-mod-replace")
	noRecursive := flags.Bool("no-recursive", false, "only scan go.mod files passed on the command line")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	paths, err := inputLines(flags.Args(), os.Stdin)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(
		attribute.Int("go_includes.seed_go_mod_count", len(paths)),
		attribute.Bool("go_includes.recursive", !*noRecursive),
	)
	includes, rerr = goModIncludes(ctx, paths, !*noRecursive, sourceFileContents)
	span.SetAttributes(attribute.Int("go_includes.include_count", len(includes)))
	return includes, rerr
}

func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ExitOnError)
	flags.Usage = func() {
		usage()
		flags.PrintDefaults()
	}
	return flags
}

func inputLines(args []string, stdin io.Reader) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}

	var paths []string
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, scanner.Err()
}

func goDirectiveIncludes(filePath, prefix string) ([]string, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return goDirectiveIncludesFromBytes(filePath, prefix, data)
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
	filePath = strings.TrimPrefix(filePath, sourceRoot+"/")
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
