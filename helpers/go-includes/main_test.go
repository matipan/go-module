package main

import (
	"reflect"
	"testing"
)

func TestGoDirectiveIncludesByMode(t *testing.T) {
	data := []byte(`package includes

import "embed"

//go:test:include local.txt "../shared file.txt" ` + "`raw path.txt`" + `
//go:generate:include generator.txt
//go:embed assets/*.tmpl all:hidden
//go:generate go -C .. run ./cmd/codegen
var embedded embed.FS
`)

	gotLint, err := goDirectiveIncludesFromBytes("pkg/includes_test.go", data, modeLint)
	if err != nil {
		t.Fatal(err)
	}
	wantLint := directiveScan{
		includes: []string{
			"pkg/assets/*.tmpl",
			"pkg/hidden",
		},
	}
	if !reflect.DeepEqual(gotLint, wantLint) {
		t.Fatalf("lint includes mismatch:\n got: %#v\nwant: %#v", gotLint, wantLint)
	}

	gotTest, err := goDirectiveIncludesFromBytes("pkg/includes_test.go", data, modeTest)
	if err != nil {
		t.Fatal(err)
	}
	wantTest := directiveScan{
		includes: []string{
			"pkg/local.txt",
			"pkg/../shared file.txt",
			"pkg/raw path.txt",
			"pkg/assets/*.tmpl",
			"pkg/hidden",
		},
	}
	if !reflect.DeepEqual(gotTest, wantTest) {
		t.Fatalf("test includes mismatch:\n got: %#v\nwant: %#v", gotTest, wantTest)
	}

	gotGenerate, err := goDirectiveIncludesFromBytes("pkg/includes_test.go", data, modeGenerate)
	if err != nil {
		t.Fatal(err)
	}
	wantGenerate := directiveScan{
		includes: []string{
			"pkg/generator.txt",
			"pkg/assets/*.tmpl",
			"pkg/hidden",
		},
		modules: []string{"."},
	}
	if !reflect.DeepEqual(gotGenerate, wantGenerate) {
		t.Fatalf("generate includes mismatch:\n got: %#v\nwant: %#v", gotGenerate, wantGenerate)
	}

	gotCombined, err := goDirectiveIncludesFromBytes("pkg/includes_test.go", data, includeModesFromFlags(false, true, true))
	if err != nil {
		t.Fatal(err)
	}
	wantCombined := directiveScan{
		includes: []string{
			"pkg/local.txt",
			"pkg/../shared file.txt",
			"pkg/raw path.txt",
			"pkg/generator.txt",
			"pkg/assets/*.tmpl",
			"pkg/hidden",
		},
		modules: []string{"."},
	}
	if !reflect.DeepEqual(gotCombined, wantCombined) {
		t.Fatalf("combined includes mismatch:\n got: %#v\nwant: %#v", gotCombined, wantCombined)
	}
}

func TestGoModLocalReplaceModulePaths(t *testing.T) {
	got, err := goModLocalReplaceModulePaths("app/go.mod", []byte(`module example.com/app

go 1.25

replace example.com/lib => ./lib
replace example.com/parent => ../parent
replace example.com/versioned => example.com/versioned v1.2.3
`))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"app/lib",
		"parent",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replace module paths mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestIncludeHelpers(t *testing.T) {
	if got := includeModesFromFlags(false, false, false); !reflect.DeepEqual(got, modeTest) {
		t.Fatalf("default include mode got %#v, want %#v", got, modeTest)
	}
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
	_, err := includePatternsFromComment(`//go:test:include "unterminated`, modeTest)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNonDirectiveCommentsAreIgnored(t *testing.T) {
	tests := []string{
		"// go:embed assets",
		"// go:generate go -C . run ./cmd/codegen",
		"//go:test:included assets",
		"//go:generate:included assets",
		"//go:generate:include assets",
		"// workspace:include assets",
		"/* go:test:include assets */",
	}
	for _, test := range tests {
		got, err := includePatternsFromComment(test, modeTest)
		if err != nil {
			t.Fatalf("%q: %v", test, err)
		}
		if len(got.includes) != 0 || len(got.modules) != 0 {
			t.Fatalf("%q: got includes %#v", test, got)
		}
	}
}
