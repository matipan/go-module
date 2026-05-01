package main

import (
	"os"
	"path/filepath"
	"reflect"
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
	dir := t.TempDir()
	filePath := filepath.Join(dir, "go.mod")
	err := os.WriteFile(filePath, []byte(`module example.com/app

go 1.25

replace example.com/lib => ../lib
replace example.com/other v1.2.3 => ./other
replace example.com/remote => example.com/fork v1.2.3
replace example.com/absolute => /tmp/absolute
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	got, err := goModIncludes(filePath, "app")
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

func TestAbsoluteIncludesIgnorePrefix(t *testing.T) {
	got := addIncludePrefix("pkg", "/from-root.txt")
	if got != "from-root.txt" {
		t.Fatalf("unexpected include: %q", got)
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
