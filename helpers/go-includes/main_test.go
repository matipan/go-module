package main

import (
	"context"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestGoDirectiveIncludes(t *testing.T) {
	got, err := goDirectiveIncludesFromBytes("pkg/includes_test.go", "pkg", []byte(`package includes

import "embed"

// workspace:include local.txt "../shared file.txt" `+"`raw path.txt`"+`
//go:embed assets/*.tmpl all:hidden
//go:generate go -C ../../ run ./cmd/codegen
var embedded embed.FS
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pkg/local.txt",
		"pkg/../shared file.txt",
		"pkg/raw path.txt",
		"pkg/assets/*.tmpl",
		"pkg/hidden",
		"pkg/../../**/*.go",
		"pkg/../../**/*.c",
		"pkg/../../**/*.h",
		"pkg/../../**/*.s",
		"pkg/../../**/*.S",
		"pkg/../../**/*.syso",
		"pkg/../../go.mod",
		"pkg/../../go.sum",
		"pkg/../../go.work",
		"pkg/../../go.work.sum",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestIncludePrefixForGoFile(t *testing.T) {
	tests := map[string]string{
		"/pkg/file.go":       "pkg",
		"pkg/nested/file.go": "pkg/nested",
		"/file.go":           "",
	}
	for filePath, want := range tests {
		if got := includePrefixForGoFile(filePath); got != want {
			t.Fatalf("%s: got %q, want %q", filePath, got, want)
		}
	}
}

func TestModuleIncludesDiscoversDirectivesAndGoModReplaces(t *testing.T) {
	ws := staticWorkspace{
		"app/go.mod": `module example.com/app

go 1.25

replace example.com/lib => ./lib
`,
		"app/main.go": `package app

import "embed"

// workspace:include data.txt
//go:embed assets/*.txt
//go:generate go -C ./tools run ./cmd/codegen
var assets embed.FS
`,
		"app/lib/go.mod": `module example.com/lib

go 1.25

replace example.com/leaf => ./leaf
`,
		"app/lib/leaf/go.mod": `module example.com/leaf

go 1.25
`,
		"app/tools/go.mod": `module example.com/tools

go 1.25
`,
	}
	got, err := moduleDiscovery{
		workspace:  ws,
		modulePath: "app",
		recursive:  true,
	}.includes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"app/data.txt",
		"app/assets/*.txt",
		"app/./tools/**/*.go",
		"app/./tools/**/*.c",
		"app/./tools/**/*.h",
		"app/./tools/**/*.s",
		"app/./tools/**/*.S",
		"app/./tools/**/*.syso",
		"app/./tools/go.mod",
		"app/./tools/go.sum",
		"app/./tools/go.work",
		"app/./tools/go.work.sum",
		"app/./lib/**",
		"app/lib/./leaf/**",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestModuleIncludesSkipsGoLikeDirectories(t *testing.T) {
	ws := goDirectoryWorkspace{
		staticWorkspace: staticWorkspace{
			"app/go.mod": `module example.com/app

go 1.25
`,
			"app/main.go": `package app

// workspace:include data.txt
`,
		},
		dir: "app/templates/src/_dagger.gen.go",
	}
	got, err := moduleDiscovery{
		workspace:  ws,
		modulePath: "app",
		recursive:  true,
	}.includes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app/data.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestGenerateModules(t *testing.T) {
	ws := staticWorkspace{
		"go.mod": `module example.com/root

go 1.25
`,
		"main.go": `package root
`,
		"app/go.mod": `module example.com/app

go 1.25
`,
		"app/generate.go": `package app

//go:generate go run ./cmd/codegen
`,
		"app/nested/go.mod": `module example.com/nested

go 1.25
`,
		"app/nested/generate.go": `package nested

//go:generate go run ./cmd/codegen
`,
	}

	got, err := generateModules(context.Background(), ws, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app", "app/nested"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modules mismatch:\n got: %#v\nwant: %#v", got, want)
	}

	got, err = generateModules(context.Background(), ws, "app")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered modules mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestAbsoluteIncludesIgnorePrefix(t *testing.T) {
	got := addIncludePrefix("pkg", "/from-root.txt")
	if got != "from-root.txt" {
		t.Fatalf("unexpected include: %q", got)
	}
}

type staticWorkspace map[string]string

type goDirectoryWorkspace struct {
	staticWorkspace
	dir string
}

func (w goDirectoryWorkspace) directory(include, exclude []string) workspaceDirectory {
	return goDirectory{
		staticDirectory: staticDirectory{
			files:   w.staticWorkspace,
			include: include,
			exclude: exclude,
		},
		dir: w.dir,
	}
}

type goDirectory struct {
	staticDirectory
	dir string
}

func (d goDirectory) glob(ctx context.Context, pattern string) ([]string, error) {
	matches, err := d.staticDirectory.glob(ctx, pattern)
	if err != nil {
		return nil, err
	}
	if matchAny(d.include, d.dir) && !matchAny(d.exclude, d.dir) && matchTestPattern(pattern, d.dir) {
		matches = append(matches, d.dir)
	}
	sort.Strings(matches)
	return matches, nil
}

func (d goDirectory) readFile(ctx context.Context, filePath string) ([]byte, error) {
	if path.Clean(filePath) == d.dir {
		return nil, errNotRegularFile
	}
	return d.staticDirectory.readFile(ctx, filePath)
}

func (w staticWorkspace) directory(include, exclude []string) workspaceDirectory {
	return staticDirectory{
		files:   w,
		include: include,
		exclude: exclude,
	}
}

type staticDirectory struct {
	files   staticWorkspace
	include []string
	exclude []string
}

func (d staticDirectory) readFile(_ context.Context, filePath string) ([]byte, error) {
	data, ok := d.files[path.Clean(filePath)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(data), nil
}

func (d staticDirectory) search(_ context.Context, pattern string, globs []string) ([]string, error) {
	var filePaths []string
	for filePath := range d.files {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)

	var matches []string
	for _, filePath := range filePaths {
		if !matchAny(d.include, filePath) || matchAny(d.exclude, filePath) || !matchAny(globs, filePath) {
			continue
		}
		if strings.Contains(d.files[filePath], pattern) {
			matches = append(matches, filePath)
		}
	}
	return matches, nil
}

func (d staticDirectory) glob(_ context.Context, pattern string) ([]string, error) {
	var matches []string
	for filePath := range d.files {
		if matchAny(d.include, filePath) && !matchAny(d.exclude, filePath) && matchTestPattern(pattern, filePath) {
			matches = append(matches, filePath)
		}
	}
	sort.Strings(matches)
	return matches, nil
}

func matchAny(patterns []string, filePath string) bool {
	for _, pattern := range patterns {
		if matchTestPattern(pattern, filePath) {
			return true
		}
	}
	return false
}

func matchTestPattern(pattern, filePath string) bool {
	pattern = path.Clean(pattern)
	filePath = path.Clean(filePath)
	if pattern == filePath {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(filePath, strings.TrimSuffix(pattern, "/**")+"/")
	}
	if strings.HasSuffix(pattern, "/**/*.go") {
		return strings.HasPrefix(filePath, strings.TrimSuffix(pattern, "/**/*.go")+"/") && strings.HasSuffix(filePath, ".go")
	}
	if strings.HasSuffix(pattern, "/**/go.mod") {
		return strings.HasPrefix(filePath, strings.TrimSuffix(pattern, "/**/go.mod")+"/") && strings.HasSuffix(filePath, "/go.mod")
	}
	if pattern == "**/*.go" {
		return strings.HasSuffix(filePath, ".go")
	}
	if pattern == "**/go.mod" {
		return filePath == "go.mod" || strings.HasSuffix(filePath, "/go.mod")
	}
	return false
}

func TestInvalidQuotedDirectiveArg(t *testing.T) {
	_, err := includePatternsFromComment(`// workspace:include "unterminated`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNonDirectiveCommentsAreIgnored(t *testing.T) {
	tests := []string{
		"// go:embed assets",
		"// go:generate go -C . run ./cmd/codegen",
		"// workspace:included assets",
		"/* workspace:include assets */",
	}
	for _, test := range tests {
		got, err := includePatternsFromComment(test)
		if err != nil {
			t.Fatalf("%q: %v", test, err)
		}
		if len(got) != 0 {
			t.Fatalf("%q: got includes %#v", test, got)
		}
	}
}
