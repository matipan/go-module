package crossincludeb

import (
	"errors"
	"os"
	"testing"
)

func TestSiblingModuleDirectiveIncludeIsNotMounted(t *testing.T) {
	_, err := os.Stat("../go-module-cross-include-a/module-a-only.data")
	if err == nil {
		t.Fatal("sibling module directive include was mounted")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
