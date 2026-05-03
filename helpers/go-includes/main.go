// go.dang runs this helper inside the mounted source tree.
// It emits workspace include patterns, one per line.
package main

import (
	"context"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
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

	var (
		includes []string
		err      error
	)
	switch os.Args[1] {
	case "source":
		includes, err = runSource(ctx, os.Args[2:])
	case "gomod":
		includes, err = runGoMod(ctx, os.Args[2:])
	case "imports":
		includes, err = runImports(ctx, os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "  go-includes source --add-prefix=DIR -- FILE.go")
	fmt.Fprintln(os.Stderr, "  go-includes gomod  [--no-recursive] -- go.mod [go.mod...]")
	fmt.Fprintln(os.Stderr, "  go-includes imports -- include-pattern [include-pattern...]")
}

func runSource(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes source")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags("source")
	prefix := flags.String("add-prefix", "", "prefix to add to relative include patterns")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		os.Exit(2)
	}
	span.SetAttributes(attribute.String("go_includes.file", flags.Arg(0)))
	includes, rerr = sourceIncludes(flags.Arg(0), *prefix)
	span.SetAttributes(attribute.Int("go_includes.include_count", len(includes)))
	return includes, rerr
}

func runGoMod(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes gomod")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags("gomod")
	noRecursive := flags.Bool("no-recursive", false, "only scan go.mod files passed on the command line")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() == 0 {
		flags.Usage()
		os.Exit(2)
	}
	span.SetAttributes(
		attribute.Int("go_includes.seed_go_mod_count", flags.NArg()),
		attribute.Bool("go_includes.recursive", !*noRecursive),
	)
	includes, rerr = goModIncludes(ctx, flags.Args(), !*noRecursive, sourceFileContents)
	span.SetAttributes(attribute.Int("go_includes.include_count", len(includes)))
	return includes, rerr
}

func runImports(ctx context.Context, args []string) (includes []string, rerr error) {
	ctx, span := otel.Tracer("go-includes").Start(ctx, "go-includes imports")
	defer telemetry.EndWithCause(span, &rerr)

	flags := newFlags("imports")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() == 0 {
		flags.Usage()
		os.Exit(2)
	}
	goMods, goFiles, err := workspaceGoSeeds(ctx, flags.Args())
	if err != nil {
		return nil, err
	}
	span.SetAttributes(
		attribute.Int("go_includes.seed_include_count", flags.NArg()),
		attribute.Int("go_includes.seed_go_mod_count", len(goMods)),
		attribute.Int("go_includes.seed_go_file_count", len(goFiles)),
	)
	includes, rerr = localImportIncludes(ctx, goMods, goFiles, sourceFileContents, sourceGlob)
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

func sourceIncludes(filePath, prefix string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
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
