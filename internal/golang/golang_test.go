package golang

import "testing"

func TestModuleLabel(t *testing.T) {
	tests := []struct {
		name  string
		root  string
		dir   string
		multi bool
		want  string
	}{
		// A single module keeps the bare tool names, so existing output and
		// anything parsing it are unaffected by this file's existence.
		{"single module is unlabelled", "/repo", "/repo/wctl", false, ""},
		{"module at the root is unlabelled", "/repo", "/repo", true, ""},
		// Relative, not absolute: an absolute path would put the runner's
		// workspace directory into output that gets compared across runs.
		{"relative to root", "/repo", "/repo/wctl", true, "wctl: "},
		{"nested relative to root", "/repo", "/repo/source/sdk/wardnet-go", true, "source/sdk/wardnet-go: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := moduleLabel(tc.root, tc.dir, tc.multi); got != tc.want {
				t.Fatalf("moduleLabel(%q, %q, %v) = %q, want %q", tc.root, tc.dir, tc.multi, got, tc.want)
			}
		})
	}
}
