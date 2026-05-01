package crossincludea

import (
	"os"
	"strings"
	"testing"
)

func TestCrossModuleWorkspaceInclude(t *testing.T) {
	contents, err := os.ReadFile("../shared-cross-module.data")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(contents)) != "shared across modules" {
		t.Fatalf("unexpected shared data: %q", contents)
	}

	contents, err = os.ReadFile("../shared file.data")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(contents)) != "shared path with spaces" {
		t.Fatalf("unexpected shared data: %q", contents)
	}
}
