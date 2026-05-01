package gomodulewithreplace

import (
	"testing"

	replacedlib "example.com/replaced-lib"
)

func TestLocalReplaceAssetsAreMounted(t *testing.T) {
	if got := replacedlib.Asset(); got != "asset from replaced module" {
		t.Fatalf("unexpected asset: %q", got)
	}
}
