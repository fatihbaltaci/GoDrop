package updater

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// withLookup replaces the package-manager probe for one test.
func withLookup(t *testing.T, fn func(string, ...string) error) {
	t.Helper()
	original := lookup
	lookup = fn
	t.Cleanup(func() { lookup = original })
}

func TestManagedByDescribesHowToRemoveItToo(t *testing.T) {
	// An installation somebody else owns is refused by both update and
	// uninstall, so both need the command that does the job properly.
	original := osExecutable
	t.Cleanup(func() { osExecutable = original })

	osExecutable = func() (string, error) { return "", errors.New("no executable") }
	if _, ok := ManagedBy(); ok {
		t.Error("without a path there is nothing to look up")
	}

	osExecutable = func() (string, error) { return filepath.Join(t.TempDir(), "godrop"), nil }
	withLookup(t, func(string, ...string) error { return errors.New("not installed") })
	if _, ok := ManagedBy(); ok {
		t.Error("an ordinary installation belongs to nobody")
	}

	withLookup(t, func(name string, _ ...string) error {
		if name == "dpkg" {
			return nil
		}
		return errors.New("not installed")
	})
	got, ok := ManagedBy()
	if !ok || got.Name != "dpkg" {
		t.Fatalf("ManagedBy = %+v, %t", got, ok)
	}
	if !strings.Contains(got.Update, "apt install") || !strings.Contains(got.Remove, "apt remove") {
		t.Errorf("commands = %+v", got)
	}
}

func TestRemoveCommandPerManager(t *testing.T) {
	cases := map[string]string{
		"dpkg":              "apt remove",
		"rpm":               "dnf remove",
		"Homebrew":          "brew uninstall",
		"a container image": "docker rm",
	}
	for manager, want := range cases {
		if got := removeCommand(manager); !strings.Contains(got, want) {
			t.Errorf("removeCommand(%q) = %q, want it to mention %q", manager, got, want)
		}
	}
}
