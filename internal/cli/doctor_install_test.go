package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/wizard"
)

// installAt writes a configuration into the home directory this test run uses,
// which is where the commands look for one.
func installAt(t *testing.T, deployment, baseURL string) string {
	t.Helper()
	// Commands that end in the same verification setup runs have nothing to
	// wait for here: no service is listening.
	original := healthWait
	healthWait = 10 * time.Millisecond
	t.Cleanup(func() { healthWait = original })

	home := t.TempDir()
	setHome(t, home)
	dir := configDirForTest(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	a := wizard.Defaults()
	a.Deployment = deployment
	a.BaseURL = baseURL
	a.Token = "gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
	if deployment != wizard.DeployCompose {
		a.DataDir = filepath.Join(home, "data")
		if err := os.MkdirAll(a.DataDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wizard.Finalise(&a)
	if _, err := wizard.Write(dir, wizard.Files(a, "/usr/local/bin/godrop"), false); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDoctorFindsTheContainerInstallation(t *testing.T) {
	// A shell has none of the service's environment, so without reading the
	// generated .env the round trip is done with no token at all: exactly the
	// 401 an operator sees after a compose install.
	var authorised bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorised = r.Header.Get("Authorization") == "Bearer gd_a1b2c3d4e5f60718293a4b5c6d7e8f90"
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	defer srv.Close()

	// The container answers for its own files; this machine answers for the
	// network. One report comes out of the two.
	tooling := &stubTooling{
		found:     map[string]bool{"docker": true},
		container: "godrop-godrop-1",
		says: map[string]string{
			"doctor": `{"ok":true,"checks":[` +
				`{"group":"config","name":"tokens","status":"pass","detail":"1 token(s) configured"},` +
				`{"group":"storage","name":"data_dir_perms","status":"pass","detail":"0700"},` +
				`{"group":"end_to_end","name":"upload","status":"pass","detail":"inside the container"},` +
				`{"group":"network","name":"listening","status":"pass","detail":"inside the container"}]}`,
		},
	}
	tooling.install(t)
	dir := installAt(t, wizard.DeployCompose, srv.URL)
	_, out, _ := run(t, testBuild(), "doctor", "--offline")

	if !authorised {
		t.Error("the token from the generated .env should have been used")
	}
	if !strings.Contains(out, "runs in a container") || !strings.Contains(out, dir) {
		t.Errorf("output should say which installation it found:\n%s", out)
	}
	// From inside: the files. From here: the round trip over HTTP.
	if !strings.Contains(out, "1 token(s) configured") || !strings.Contains(out, "0700") {
		t.Errorf("the container's own checks should be in the report:\n%s", out)
	}
	if !strings.Contains(out, "upload") {
		t.Errorf("the round trip belongs to this side:\n%s", out)
	}
	// The container's idea of the network, and of a round trip to itself, is
	// about its own namespace: both of those questions are answered from here,
	// and answered once.
	if strings.Contains(out, "inside the container") {
		t.Errorf("network and round-trip checks should come from this machine:\n%s", out)
	}
	if strings.Count(out, "upload") != 1 {
		t.Errorf("the round trip should be reported once:\n%s", out)
	}
	if strings.Contains(out, "docker compose") {
		t.Errorf("the diagnosis was run, not printed:\n%s", out)
	}
}

func TestDoctorCarriesOnWhenTheContainerCannotAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	defer srv.Close()

	// Docker is not installed, so the parts only the container can answer for
	// are missing; the parts this machine can answer for are not.
	tooling := &stubTooling{found: map[string]bool{}}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, srv.URL)

	_, out, _ := run(t, testBuild(), "doctor", "--offline")
	if !strings.Contains(out, "could not diagnose inside the container") {
		t.Errorf("output should say what it could not do:\n%s", out)
	}
	if !strings.Contains(out, "upload") {
		t.Errorf("the rest of the report should still be there:\n%s", out)
	}
}

func TestAnAnswerFromTheContainerThatIsNotAReport(t *testing.T) {
	tooling := &stubTooling{
		found:     map[string]bool{"docker": true},
		container: "godrop-godrop-1",
		says:      map[string]string{"doctor": "not json"},
	}
	tooling.install(t)
	installAt(t, wizard.DeployCompose, "")

	_, out, _ := run(t, testBuild(), "doctor", "--offline")
	if !strings.Contains(out, "could not read the answer from the container") {
		t.Errorf("output = %s", out)
	}
}

func TestDoctorReadsTheEnvFileOfALocalInstallation(t *testing.T) {
	dir := installAt(t, wizard.DeployEnv, "")
	_, out, _ := run(t, testBuild(), "doctor", "--offline")

	if !strings.Contains(out, filepath.Join(dir, ".env")) {
		t.Errorf("output should name the configuration it used:\n%s", out)
	}
	// The token in that file is the one the service uses, so it is not "0
	// token(s) configured" whatever this shell has exported.
	if strings.Contains(out, "0 token(s)") {
		t.Errorf("the generated .env carries a token:\n%s", out)
	}
}

func TestDoctorLeavesAnExplicitURLAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	defer srv.Close()
	installAt(t, wizard.DeployCompose, "https://files.example.com")

	_, out, _ := run(t, testBuild(), "doctor", "--offline", "--url", srv.URL, "--token", "gd_explicit")
	if strings.Contains(out, "runs in a container") {
		t.Errorf("--url means the operator has already said what to look at:\n%s", out)
	}
}

func TestTheEnvironmentWinsOverTheGeneratedFile(t *testing.T) {
	t.Parallel()
	env := withEnvFile(func(key string) string {
		if key == "GODROP_ADDR" {
			return ":9000"
		}
		return ""
	}, map[string]string{"GODROP_ADDR": ":8747", "GODROP_TOKENS": "gd_from_the_file"})

	if got := env("GODROP_ADDR"); got != ":9000" {
		t.Errorf("GODROP_ADDR = %q, want what this shell exported", got)
	}
	if got := env("GODROP_TOKENS"); got != "gd_from_the_file" {
		t.Errorf("GODROP_TOKENS = %q, want the file to fill the gap", got)
	}
}

func TestTheTokenCommandFitsTheDeployment(t *testing.T) {
	t.Parallel()
	// Even where the file is in a volume, the command is the same one: it
	// reaches the file there by itself.
	compose := tokenCommand("/home/you/.godrop", wizard.DeployCompose, "")
	if !strings.HasSuffix(compose, "token create --name claude-code") || strings.Contains(compose, "docker") {
		t.Errorf("compose = %q", compose)
	}
	local := tokenCommand("/home/you/.godrop", wizard.DeploySystemd, "/var/lib/godrop")
	if !strings.Contains(local, "token create --data-dir /var/lib/godrop --name claude-code") {
		t.Errorf("a local installation names its data directory: %q", local)
	}
	if got := tokenCommand("/x", wizard.DeployEnv, ""); !strings.HasSuffix(got, "token create --name claude-code") {
		t.Errorf("without a data directory the default applies: %q", got)
	}
}

func TestTheCommandIsAPathWhenGoDropIsNotOnThePath(t *testing.T) {
	missing := &stubTooling{found: map[string]bool{}}
	missing.install(t)
	original := osExecutable
	osExecutable = func() (string, error) { return "/opt/godrop/godrop", nil }
	t.Cleanup(func() { osExecutable = original })
	if got := godropCommand(); got != "/opt/godrop/godrop" {
		t.Errorf("godropCommand = %q, want the full path", got)
	}

	// Nothing to say where this binary is: the name is the best guess left.
	osExecutable = func() (string, error) { return "", errAlreadyThere }
	if got := godropCommand(); got != "godrop" {
		t.Errorf("godropCommand = %q", got)
	}

	found := &stubTooling{found: map[string]bool{"godrop": true}}
	found.install(t)
	if got := godropCommand(); got != "godrop" {
		t.Errorf("godropCommand = %q, want the plain name", got)
	}
}

func TestTheTryItLineUsesThisInstancesAddress(t *testing.T) {
	t.Setenv("GODROP_BASE_URL", "https://files.example.com/")
	if got := localAddress(); got != "https://files.example.com" {
		t.Errorf("localAddress = %q", got)
	}
	t.Setenv("GODROP_BASE_URL", "")
	t.Setenv("GODROP_ADDR", ":48151")
	if got := localAddress(); got != "http://localhost:48151" {
		t.Errorf("localAddress = %q", got)
	}
	t.Setenv("GODROP_ADDR", "")
	if got := localAddress(); got != "http://localhost:8747" {
		t.Errorf("localAddress = %q, want the default port", got)
	}
	// An address with no colon in it is not one net.SplitHostPort can read.
	t.Setenv("GODROP_ADDR", "sock")
	if got := localAddress(); got != "http://localhostsock" {
		t.Errorf("localAddress = %q", got)
	}
}
