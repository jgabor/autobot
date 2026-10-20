package task

import (
	"path/filepath"
	"testing"
)

// The examples in the repository must stay valid tasks.
func TestExamplesParse(t *testing.T) {
	files, err := filepath.Glob("../../examples/tasks/*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, f := range files {
		if _, err := Load(f, defaults); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
