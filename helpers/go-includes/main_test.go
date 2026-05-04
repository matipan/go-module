package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

func TestInputPathsUsesArgs(t *testing.T) {
	got, err := inputPaths([]string{"a.go", "b.go"}, strings.NewReader("ignored.go\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.go", "b.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestInputPathsReadsStdin(t *testing.T) {
	got, err := inputPaths(nil, strings.NewReader("a.go\n\nb.go\r\n"))
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
