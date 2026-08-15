package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The README reproduces the binary's own help output. Documentation that
// quietly stops matching the tool is worse than none, so the two are compared
// here rather than trusted.
func TestReadmeCommandLineIsUpToDate(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)

	for _, args := range [][]string{
		{},
		{"serve"},
		{"init"},
		{"token"},
		{"token", "create"},
		{"token", "list"},
		{"token", "revoke"},
		{"doctor"},
		{"telemetry"},
		{"health"},
		{"version"},
	} {
		var out strings.Builder
		if code := ExecuteWith(t.Context(), testBuild(), append(args, "--help"), &out, &out); code != 0 {
			t.Fatalf("%v --help exited %d", args, code)
		}
		label := strings.TrimSpace("godrop " + strings.Join(args, " "))
		if !strings.Contains(text, out.String()) {
			t.Errorf("the README does not show the current `%s --help` output.\n"+
				"Run: make docs\n\nGot:\n%s", label, out.String())
		}
	}
}
