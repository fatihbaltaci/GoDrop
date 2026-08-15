package doctor

import (
	"bytes"
	"io"
	"mime/multipart"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/fatihbaltaci/GoDrop/internal/telemetry"
)

// targetURL is the address the diagnosis should probe: an explicit --url, the
// configured base URL, or the local listen address as a last resort.
func (r *runner) targetURL() string {
	if r.TargetURL != "" {
		return strings.TrimRight(r.TargetURL, "/")
	}
	if r.Config == nil {
		return ""
	}
	if r.Config.BaseURL != "" {
		return strings.TrimRight(r.Config.BaseURL, "/")
	}
	return "http://" + localAddr(r.Config.Addr)
}

// localAddr turns a listen address (":8080", "0.0.0.0:8080") into something
// dialable from this machine.
func localAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// portOf picks the port that the outside world connects to: the one in the
// public URL when it has one, otherwise the local listen port.
func portOf(listenAddr, target string) int {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				return n
			}
		} else if u.Scheme == "https" {
			return 443
		} else if u.Scheme == "http" {
			return 80
		}
	}
	if _, p, err := net.SplitHostPort(listenAddr); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 0
}

func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func slicesContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// multipartBody builds a one-file multipart payload for the round-trip check.
// Every write goes to a bytes.Buffer, which cannot fail.
func multipartBody(filename string, content []byte) (io.Reader, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, _ := w.CreateFormFile("file", filename)
	_, _ = part.Write(content)
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

// diskFree and readMounts are variables so the "almost full" and "no volume
// mounted" situations can be diagnosed in tests without arranging a full disk
// or a container.
var (
	diskFree   = statfs
	readMounts = func() ([]byte, error) { return os.ReadFile("/proc/mounts") }
)

// gitTracked reports whether a path is tracked in the repository at dir. A
// missing git binary simply reports "not tracked".
var gitTracked = func(dir, path string) bool {
	cmd := exec.Command("git", "ls-files", "--error-unmatch", path)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// inContainer reports whether we are running inside a container, reusing the
// platform detection the telemetry package already performs.
var inContainer = func() bool {
	switch telemetry.DetectDeploy(os.Getenv) {
	case "docker", "kubernetes":
		return true
	}
	return false
}

// geteuid is a variable so the "running as root" branch can be diagnosed
// without actually being root.
var geteuid = os.Geteuid
