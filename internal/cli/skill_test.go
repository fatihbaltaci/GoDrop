package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The skill is what a coding agent reads to learn how to use GoDrop, so the
// copy the binary carries has to be the one the repository publishes at
// skills/godrop/SKILL.md, which is where `gh skill install` looks for it.
func TestTheEmbeddedSkillMatchesTheOneInTheRepository(t *testing.T) {
	published, err := os.ReadFile(filepath.Join("..", "..", "skills", "godrop", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if normalise(string(published)) != normalise(skillDoc) {
		t.Error("the embedded skill and skills/godrop/SKILL.md have drifted apart.\nRun: make docs")
	}
	if !strings.HasPrefix(normalise(skillDoc), "---\nname: godrop") {
		t.Error("a skill needs frontmatter naming it, or no agent will find it")
	}
}

func normalise(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func TestSkillInstallWritesWhereTheAgentLooks(t *testing.T) {
	dir := t.TempDir()
	code, out, stderr := run(t, testBuild(), "skill", "install", "--dir", dir, "--json")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	var got struct{ Path, Skill string }
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	want := filepath.Join(dir, "godrop", "SKILL.md")
	if got.Path != want || got.Skill != "godrop" {
		t.Errorf("result = %+v, want %q", got, want)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != skillDoc {
		t.Error("the installed file should be the skill itself")
	}
	// No secret may end up in a file agents copy around and commit.
	for _, forbidden := range []string{"gd_a1b2", "Bearer gd_"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("the skill contains %q", forbidden)
		}
	}
}

func TestSkillInstallRefusesToClobberWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if code, _, stderr := run(t, testBuild(), "skill", "install", "--dir", dir); code != 0 {
		t.Fatalf("first install: %s", stderr)
	}
	path := filepath.Join(dir, "godrop", "SKILL.md")
	if err := os.WriteFile(path, []byte("edited by hand"), 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}

	code, _, stderr := run(t, testBuild(), "skill", "install", "--dir", dir)
	if code == 0 || !strings.Contains(stderr, "--force") {
		t.Errorf("a second install should refuse: exit %d, %q", code, stderr)
	}
	if data, _ := os.ReadFile(path); string(data) != "edited by hand" {
		t.Error("the existing file must be left alone")
	}

	if code, _, stderr := run(t, testBuild(), "skill", "install", "--dir", dir, "--force"); code != 0 {
		t.Fatalf("--force should overwrite: %s", stderr)
	}
	if data, _ := os.ReadFile(path); string(data) != skillDoc {
		t.Error("--force should have replaced the file")
	}
}

func TestSkillInstallKnowsTheAgentDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows

	cases := []struct {
		agent, scope string
		want         string
	}{
		{"shared", "project", filepath.Join(".agents", "skills", "godrop")},
		{"claude", "project", filepath.Join(".claude", "skills", "godrop")},
		{"shared", "user", filepath.Join(home, ".agents", "skills", "godrop")},
		{"CLAUDE", "USER", filepath.Join(home, ".claude", "skills", "godrop")},
	}
	for _, tc := range cases {
		dir, err := skillDir(tc.agent, tc.scope, "")
		if err != nil {
			t.Fatalf("skillDir(%q, %q): %v", tc.agent, tc.scope, err)
		}
		if got := filepath.Join(dir, "godrop"); got != tc.want {
			t.Errorf("skillDir(%q, %q) = %q, want %q", tc.agent, tc.scope, got, tc.want)
		}
	}
	if _, err := skillDir("emacs", "project", ""); err == nil {
		t.Error("an unknown agent should be reported rather than guessed at")
	}
	if _, err := skillDir("shared", "global", ""); err == nil {
		t.Error("an unknown scope should be reported")
	}
	if dir, _ := skillDir("nonsense", "nonsense", "/tmp/explicit"); dir != "/tmp/explicit" {
		t.Errorf("--dir should win: %q", dir)
	}
}

func TestSkillInstallReportsAnUnwritableDirectory(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if code, _, _ := run(t, testBuild(), "skill", "install", "--dir", filepath.Join(dir, "skills")); code == 0 {
		t.Error("an unwritable directory should fail")
	}
}

func TestSkillInstallReportsAnUnreadableTarget(t *testing.T) {
	requireStrictPermissions(t)
	dir := t.TempDir()
	nested := filepath.Join(dir, "godrop")
	if err := os.MkdirAll(nested, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nested, 0o700) })

	if code, _, _ := run(t, testBuild(), "skill", "install", "--dir", dir); code == 0 {
		t.Error("a target that cannot be examined should fail")
	}
}

func TestSkillShowPrintsTheSkill(t *testing.T) {
	code, out, stderr := run(t, testBuild(), "skill", "show")
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if out != skillDoc {
		t.Error("skill show should print the skill unchanged")
	}
}

func TestSkillInstallSaysWhatItDid(t *testing.T) {
	dir := t.TempDir()
	code, out, stderr := run(t, testBuild(), "skill", "install", "--dir", dir)
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	for _, want := range []string{"installed", "godrop", "GODROP_TOKEN"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should mention %q:\n%s", want, out)
		}
	}
}

func TestSkillInstallRejectsAnUnknownAgent(t *testing.T) {
	code, _, stderr := run(t, testBuild(), "skill", "install", "--agent", "emacs")
	if code == 0 || !strings.Contains(stderr, "unknown agent") {
		t.Errorf("exit = %d, stderr = %q", code, stderr)
	}
}

func TestSkillInstallReportsAFileItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	// A directory where the skill file belongs: it exists, so --force is
	// needed, and then writing it still cannot work.
	if err := os.MkdirAll(filepath.Join(dir, "godrop", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(t, testBuild(), "skill", "install", "--dir", dir, "--force"); code == 0 {
		t.Error("writing over a directory should fail")
	}
}

func TestSkillInstallNeedsAHomeDirectoryForUserScope(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, err := skillDir("shared", "user", ""); err == nil {
		t.Error("without a home directory, user scope has nowhere to go")
	}
}
