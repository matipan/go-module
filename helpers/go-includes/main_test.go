package main

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestSourceIncludes(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "includes_test.go")
	err := os.WriteFile(filePath, []byte(`package includes

import "embed"

// workspace:include local.txt "../shared file.txt" `+"`raw path.txt`"+`
//go:embed assets/*.tmpl all:hidden
var embedded embed.FS
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	got, err := sourceIncludes(filePath, "pkg")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pkg/local.txt",
		"pkg/../shared file.txt",
		"pkg/raw path.txt",
		"pkg/assets/*.tmpl",
		"pkg/hidden",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
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

func TestLocalImportIncludes(t *testing.T) {
	files := map[string]string{
		"go.mod": `module example.com/root

go 1.25
`,
		"cmd/codegen/main.go": `package main

import "example.com/root/internal/helper"

func main() {}
`,
		"internal/helper/helper.go": `package helper

import "example.com/root/engine/slog"
`,
		"engine/slog/slog.go": `package slog
`,
		"sdk/go/go.mod": `module example.com/sdk

go 1.25
`,
		"sdk/go/client.go": `package dagger

import "example.com/sdk/querybuilder"
`,
		"sdk/go/querybuilder/querybuilder.go": `package querybuilder
`,
	}

	got, err := localImportIncludes(
		context.Background(),
		[]string{"go.mod", "sdk/go/go.mod"},
		[]string{"cmd/codegen/main.go", "sdk/go/client.go"},
		staticFiles(files),
		staticGlob(files),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/helper/**",
		"sdk/go/querybuilder/**",
		"engine/slog/**",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLocalImportIncludesSkipsGoNamedDirectories(t *testing.T) {
	files := map[string]string{
		"go.mod": `module example.com/root

go 1.25
`,
		"cmd/codegen/main.go": `package main

import "example.com/root/internal/helper"
`,
		"internal/helper/helper.go": `package helper
`,
	}
	baseReadFile := staticFiles(files)
	readFile := func(ctx context.Context, filePath string) ([]byte, error) {
		if filePath == "cmd/codegen/templates/src/_dagger.gen.go" {
			return nil, errors.New("path cmd/codegen/templates/src/_dagger.gen.go is a directory, not a file")
		}
		return baseReadFile(ctx, filePath)
	}

	got, err := localImportIncludes(
		context.Background(),
		[]string{"go.mod"},
		[]string{"cmd/codegen/main.go", "cmd/codegen/templates/src/_dagger.gen.go"},
		readFile,
		staticGlob(files),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"internal/helper/**"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("includes mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLocalImportDirModuleRoot(t *testing.T) {
	got, ok := localImportDir([]localGoModule{
		{dir: "sdk/go", path: "example.com/sdk"},
	}, "example.com/sdk")
	if !ok {
		t.Fatal("expected local import match")
	}
	if got != "sdk/go" {
		t.Fatalf("local import dir mismatch: got %q, want %q", got, "sdk/go")
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

func staticGlob(files map[string]string) workspaceGlobber {
	return func(_ context.Context, pattern string) ([]string, error) {
		var matches []string
		for filePath := range files {
			match, err := path.Match(pattern, filePath)
			if err != nil {
				return nil, err
			}
			if match {
				matches = append(matches, filePath)
			}
		}
		sort.Strings(matches)
		return matches, nil
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
