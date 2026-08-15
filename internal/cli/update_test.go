package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fatihbaltaci/GoDrop/internal/updater"
)

// stubUpdater replaces the two seams the command goes through, so the suite
// never talks to GitHub or rewrites a binary.
func stubUpdater(t *testing.T,
	latest func(context.Context, updater.Options) (string, error),
	update func(context.Context, string, updater.Options) (updater.Result, error),
) {
	t.Helper()
	originalLatest, originalRun := updateLatest, updateRun
	if latest != nil {
		updateLatest = latest
	}
	if update != nil {
		updateRun = update
	}
	t.Cleanup(func() { updateLatest, updateRun = originalLatest, originalRun })
}

func TestUpdateCheckReportsANewerRelease(t *testing.T) {
	stubUpdater(t, func(context.Context, updater.Options) (string, error) { return "v9.9.9", nil }, nil)

	code, out, stderr := run(t, testBuild(), "update", "--check")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "9.9.9") || !strings.Contains(out, "godrop update") {
		t.Errorf("the output should offer the update:\n%s", out)
	}
}

func TestUpdateCheckIsQuietWhenCurrent(t *testing.T) {
	// testBuild is 1.2.3, and a leading v must not make it look different.
	stubUpdater(t, func(context.Context, updater.Options) (string, error) { return "v1.2.3", nil }, nil)

	code, out, stderr := run(t, testBuild(), "update", "--check")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "latest release") {
		t.Errorf("output = %q", out)
	}
}

func TestUpdateCheckJSON(t *testing.T) {
	stubUpdater(t, func(context.Context, updater.Options) (string, error) { return "v9.9.9", nil }, nil)

	code, out, stderr := run(t, testBuild(), "update", "--check", "--json")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	var got struct {
		Current  string `json:"current"`
		Latest   string `json:"latest"`
		UpToDate bool   `json:"up_to_date"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if got.Current != "1.2.3" || got.Latest != "v9.9.9" || got.UpToDate {
		t.Errorf("result = %+v", got)
	}
}

func TestUpdateCheckReportsAFailedLookup(t *testing.T) {
	stubUpdater(t, func(context.Context, updater.Options) (string, error) {
		return "", errors.New("github is down")
	}, nil)

	if code, _, stderr := run(t, testBuild(), "update", "--check"); code == 0 {
		t.Errorf("a failing lookup should exit non-zero, stderr = %q", stderr)
	}
}

func TestUpdateInstallsAndSaysWhatToDoNext(t *testing.T) {
	stubUpdater(t, nil, func(_ context.Context, current string, _ updater.Options) (updater.Result, error) {
		return updater.Result{From: current, To: "v9.9.9", Path: "/usr/local/bin/godrop", Updated: true}, nil
	})

	code, out, stderr := run(t, testBuild(), "update")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	for _, want := range []string{"1.2.3", "v9.9.9", "/usr/local/bin/godrop", "restart"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should mention %q:\n%s", want, out)
		}
	}

	code, jsonOut, _ := run(t, testBuild(), "update", "--json")
	if code != 0 {
		t.Fatalf("--json exit = %d", code)
	}
	var got updater.Result
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil || !got.Updated || got.To != "v9.9.9" {
		t.Errorf("result = %q (%v)", jsonOut, err)
	}
}

func TestUpdateSaysWhenThereIsNothingToDo(t *testing.T) {
	stubUpdater(t, nil, func(_ context.Context, current string, _ updater.Options) (updater.Result, error) {
		return updater.Result{From: current, To: current, UpToDate: true}, nil
	})

	code, out, stderr := run(t, testBuild(), "update")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(out, "latest release") {
		t.Errorf("output = %q", out)
	}
}

func TestUpdateRefusesAManagedInstallation(t *testing.T) {
	managed := updater.Result{
		ManagedBy:   "dpkg",
		ManagedHint: "sudo apt install --only-upgrade godrop",
	}
	stubUpdater(t, nil, func(context.Context, string, updater.Options) (updater.Result, error) {
		return managed, fmt.Errorf("%w: dpkg. %s", updater.ErrManaged, managed.ManagedHint)
	})

	code, _, stderr := run(t, testBuild(), "update")
	if code == 0 {
		t.Fatal("a managed installation must not be overwritten")
	}
	if !strings.Contains(stderr, "apt") {
		t.Errorf("stderr should point at the package manager: %q", stderr)
	}

	// An agent asking in JSON gets the same answer as data, not an error page.
	code, out, _ := run(t, testBuild(), "update", "--json")
	if code != 0 {
		t.Fatalf("--json should describe the refusal, exit %d", code)
	}
	var got struct {
		ManagedBy string `json:"managed_by"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.ManagedBy != "dpkg" {
		t.Errorf("result = %q (%v)", out, err)
	}
}

func TestUpdateReportsAnOrdinaryFailure(t *testing.T) {
	stubUpdater(t, nil, func(context.Context, string, updater.Options) (updater.Result, error) {
		return updater.Result{}, errors.New("checksum mismatch")
	})

	code, _, stderr := run(t, testBuild(), "update", "--json")
	if code == 0 || !strings.Contains(stderr, "checksum mismatch") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestUpdateInstallsANamedVersion(t *testing.T) {
	var asked string
	stubUpdater(t, nil, func(_ context.Context, _ string, opts updater.Options) (updater.Result, error) {
		asked = opts.Version
		return updater.Result{To: opts.Version, Updated: true}, nil
	})

	if code, _, stderr := run(t, testBuild(), "update", "--version", "v1.0.0"); code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if asked != "v1.0.0" {
		t.Errorf("asked for %q, want the requested version", asked)
	}
}
