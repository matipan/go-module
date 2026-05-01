package nestedparent

import (
	"errors"
	"os"
	"testing"
)

func TestNestedModuleDirectiveIncludeIsNotMounted(t *testing.T) {
	_, err := os.Stat("nested/nested-only.data")
	if err == nil {
		t.Fatal("nested module directive include was mounted")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
