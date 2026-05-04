package skipped

import "testing"

func TestSkippedModuleIsNotRun(t *testing.T) {
	t.Fatal("this module should be skipped by an ancestor marker")
}
