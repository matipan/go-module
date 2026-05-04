package main

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestGoDirectiveIncludes(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "includes_test.go")
	err := os.WriteFile(filePath, []byte(`package includes

import "embed"

// workspace:include local.txt "../shared file.txt" `+"`raw path.txt`"+`
//go:embed assets/*.tmpl all:hidden
//go:generate go -C ../../ run ./cmd/codegen
var embedded embed.FS
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	got, err := goDirectiveIncludes(filePath, "pkg")
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

func TestGoDirectiveIncludesSkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	goDir := filepath.Join(dir, "generated.go")
	if err := os.Mkdir(goDir, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := goDirectiveIncludes(goDir, "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got includes %#v, want none", got)
	}
}

func TestIncludePrefixForGoFile(t *testing.T) {
	tests := map[string]string{
		"/src/pkg/file.go":        "pkg",
		"/src/pkg/nested/file.go": "pkg/nested",
		"/src/file.go":            "",
	}
	for filePath, want := range tests {
		if got := includePrefixForGoFile(filePath); got != want {
			t.Fatalf("%s: got %q, want %q", filePath, got, want)
		}
	}
}

func TestInputLinesUsesArgs(t *testing.T) {
	got, err := inputLines([]string{"a.go", "b.go"}, strings.NewReader("ignored.go\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.go", "b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestInputLinesReadsStdin(t *testing.T) {
	got, err := inputLines(nil, strings.NewReader("a.go\n\nb.go\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.go", "b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestGoModIncludes(t *testing.T) {
	got, err := goModIncludes(context.Background(), []string{"app/go.mod"}, false, staticGoMods(map[string]string{
		"app/go.mod": `module example.com/app

go 1.25

replace example.com/lib => ../lib
replace example.com/other v1.2.3 => ./other
replace example.com/remote => example.com/fork v1.2.3
replace example.com/absolute => /tmp/absolute
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"app/../lib/**",
		"app/./other/**",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestGoModIncludesRecursive(t *testing.T) {
	got, err := goModIncludes(context.Background(), []string{"app/go.mod"}, true, staticGoMods(map[string]string{
		"app/go.mod": `module example.com/app

go 1.25

replace example.com/lib => ../lib
`,
		"lib/go.mod": `module example.com/lib

go 1.25

replace example.com/leaf => ./leaf
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"app/../lib/**",
		"lib/./leaf/**",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestGoModIncludesStopsAtMissingRecursiveTarget(t *testing.T) {
	got, err := goModIncludes(context.Background(), []string{"app/go.mod"}, true, staticGoMods(map[string]string{
		"app/go.mod": `module example.com/app

go 1.25

replace example.com/lib => ../lib
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app/../lib/**"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
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
		seedIncludes: []string{
			"app/**/*.go",
			"app/**/go.mod",
		},
		recursive: true,
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

func TestAbsoluteIncludesIgnorePrefix(t *testing.T) {
	got := addIncludePrefix("pkg", "/from-root.txt")
	if got != "from-root.txt" {
		t.Fatalf("unexpected include: %q", got)
	}
}

func staticGoMods(files map[string]string) goModReader {
	return staticFiles(files)
}

func staticFiles(files map[string]string) goModReader {
	return func(_ context.Context, path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(data), nil
	}
}

type staticWorkspace map[string]string

func (w staticWorkspace) readFile(_ context.Context, filePath string) ([]byte, error) {
	data, ok := w[path.Clean(filePath)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(data), nil
}

func (w staticWorkspace) glob(_ context.Context, include, exclude []string, pattern string) ([]string, error) {
	var matches []string
	for filePath := range w {
		if matchAny(include, filePath) && !matchAny(exclude, filePath) && matchTestPattern(pattern, filePath) {
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
