package main

import (
	"reflect"
	"strings"
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

	gotLint, err := scanGoFileDirectives("pkg/includes_test.go", data, false, false)
	if err != nil {
		t.Fatal(err)
	}
	wantLint := discoveredInputs{
		includes: []string{
			"pkg/assets/*.tmpl",
			"pkg/hidden",
		},
	}
	if !reflect.DeepEqual(gotLint, wantLint) {
		t.Fatalf("lint includes mismatch:\n got: %#v\nwant: %#v", gotLint, wantLint)
	}

	gotTest, err := scanGoFileDirectives("pkg/includes_test.go", data, true, false)
	if err != nil {
		t.Fatal(err)
	}
	wantTest := discoveredInputs{
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

	gotGenerate, err := scanGoFileDirectives("pkg/includes_test.go", data, false, true)
	if err != nil {
		t.Fatal(err)
	}
	wantGenerate := discoveredInputs{
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

	gotCombined, err := scanGoFileDirectives("pkg/includes_test.go", data, true, true)
	if err != nil {
		t.Fatal(err)
	}
	wantCombined := discoveredInputs{
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
	if got := (targetModule{test: true}).modeString(); got != "test" {
		t.Fatalf("modeString got %q, want test", got)
	}
	if got := addIncludePrefix("pkg", "/from-root.txt"); got != "from-root.txt" {
		t.Fatalf("absolute include got %q", got)
	}
	ws := &workspace{moduleSet: map[string]bool{
		".":       true,
		"pkg/mod": true,
	}}
	if got, ok := ws.containingModuleDir("pkg/mod/subdir"); !ok || got != "pkg/mod" {
		t.Fatalf("workspace.containingModuleDir got %q, %v", got, ok)
	}

}

func TestInvalidQuotedDirectiveArg(t *testing.T) {
	_, err := (goDirective{
		position: "test.go:1:1",
		comment:  `//go:test:include "unterminated`,
	}).includes(true, false, false)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRelativeCLIPathRejected(t *testing.T) {
	_, err := newTargetModuleFromArgs(t.Context(), []string{"relative/module"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "workspace path must be absolute: relative/module") {
		t.Fatalf("unexpected error: %v", err)
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
		directive := goDirective{comment: test}
		got, err := directive.includes(true, false, false)
		if err != nil {
			t.Fatalf("%q: %v", test, err)
		}
		if len(got) != 0 || directive.isGenerateGoDashC() {
			t.Fatalf("%q: got includes %#v", test, got)
		}
	}
}
