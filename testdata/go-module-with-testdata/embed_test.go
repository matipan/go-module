package gomodulewithtestdata

import (
	"embed"
	"strings"
	"testing"
)

//go:embed embedded-assets/message.txt
var embeddedMessage string

//go:embed embedded-assets/*.tmpl
var embeddedTemplates embed.FS

//go:embed embedded-assets/nested
var embeddedNested embed.FS

//go:embed all:embedded-assets/hidden
var embeddedHidden embed.FS

func TestGoEmbedAssets(t *testing.T) {
	if strings.TrimSpace(embeddedMessage) != "mounted from go embed" {
		t.Fatalf("unexpected embedded message: %q", embeddedMessage)
	}

	tmpl, err := embeddedTemplates.ReadFile("embedded-assets/page.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(tmpl)) != "template from go embed" {
		t.Fatalf("unexpected embedded template: %q", tmpl)
	}

	nested, err := embeddedNested.ReadFile("embedded-assets/nested/value.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(nested)) != "nested from go embed" {
		t.Fatalf("unexpected embedded nested file: %q", nested)
	}

	hidden, err := embeddedHidden.ReadFile("embedded-assets/hidden/_secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(hidden)) != "hidden from go embed" {
		t.Fatalf("unexpected embedded hidden file: %q", hidden)
	}
}
