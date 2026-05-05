package main

import (
	"reflect"
	"testing"
)

func TestGoDirectiveIncludes(t *testing.T) {
	got, err := goDirectiveIncludesFromBytes("pkg/includes_test.go", []byte(`package includes

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

func TestGoModLocalReplaceIncludes(t *testing.T) {
	got, err := goModLocalReplaceIncludes("app/go.mod", []byte(`module example.com/app

go 1.25

replace example.com/lib => ./lib
replace example.com/parent => ../parent
replace example.com/versioned => example.com/versioned v1.2.3
`))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"app/./lib/**",
		"app/../parent/**",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replace includes mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestIncludeHelpers(t *testing.T) {
	if got := includePrefixForGoFile("/pkg/nested/file.go"); got != "pkg/nested" {
		t.Fatalf("includePrefixForGoFile got %q", got)
	}
	if got := addIncludePrefix("pkg", "/from-root.txt"); got != "from-root.txt" {
		t.Fatalf("absolute include got %q", got)
	}

	set := newIncludeSet("a", "", "b", "a")
	set.add("c", "b")
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(set.list, want) {
		t.Fatalf("includeSet mismatch:\n got: %#v\nwant: %#v", set.list, want)
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
