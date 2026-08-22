package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCapture runs the CLI against the test server with a clean client config
// (temp XDG_CONFIG_HOME) and returns exit code, stdout and stderr.
func runCapture(t *testing.T, stdinText string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	stdout, stderr, stdin = &out, &errb, strings.NewReader(stdinText)
	t.Cleanup(func() { stdout, stderr, stdin = os.Stdout, os.Stderr, os.Stdin })
	code := runCLI(args)
	return code, out.String(), errb.String()
}

func cliEnv(t *testing.T, api string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOP_API", api)
	t.Setenv("HOP_TOKEN", "secret")
	t.Setenv("HOP_TTL", "")
}

func TestCLIConfigPrecedence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOP_API", "")
	t.Setenv("HOP_TOKEN", "")

	// nothing: default api, no token
	c := resolveConfig("", "")
	if c.API != defaultAPI || c.Token != "" || c.tokenFrom != "none" {
		t.Fatalf("defaults: %+v", c)
	}
	// login writes the file (token from flag, api answered via stdin)
	code, _, errOut := runCapture(t, "https://go.example/\n", "login", "--token", "file-token")
	if code != 0 {
		t.Fatalf("login exit %d: %s", code, errOut)
	}
	if fi, err := os.Stat(configPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", fi, err)
	}
	if filepath.Dir(configPath()) == "" {
		t.Fatal("config path")
	}
	c = resolveConfig("", "")
	if c.API != "https://go.example" || c.Token != "file-token" || c.tokenFrom != "config" || c.apiFrom != "config" {
		t.Fatalf("from file: %+v", c)
	}
	// env beats file
	t.Setenv("HOP_API", "https://env.example")
	t.Setenv("HOP_TOKEN", "env-token")
	c = resolveConfig("", "")
	if c.API != "https://env.example" || c.Token != "env-token" || c.tokenFrom != "env" {
		t.Fatalf("env: %+v", c)
	}
	// flags beat env
	c = resolveConfig("https://flag.example", "flag-token")
	if c.API != "https://flag.example" || c.Token != "flag-token" || c.apiFrom != "flag" {
		t.Fatalf("flags: %+v", c)
	}
	// whoami masks the token
	_, out, _ := runCapture(t, "", "whoami")
	if strings.Contains(out, "env-token") || !strings.Contains(out, "env******ken") {
		t.Fatalf("whoami leaked or did not mask: %q", out)
	}
	// logout removes the file
	if code, _, _ := runCapture(t, "", "logout"); code != 0 {
		t.Fatal("logout")
	}
	if _, err := os.Stat(configPath()); !os.IsNotExist(err) {
		t.Fatal("config still there after logout")
	}
}

func TestCLILinkPasteListRemoveOpen(t *testing.T) {
	_, ts := testServer(t)
	cliEnv(t, ts.URL)

	// link with custom slug + ttl, flags after positionals
	code, out, errOut := runCapture(t, "", "link", "https://kubernetes.io/docs/", "k8s", "--ttl", "7d")
	if code != 0 || strings.TrimSpace(out) != "https://go.example/k8s" {
		t.Fatalf("link: %d %q %q", code, out, errOut)
	}
	// paste from a file with overrides
	f := filepath.Join(t.TempDir(), "notes.md")
	os.WriteFile(f, []byte("# hello\n"), 0o600)
	code, out, errOut = runCapture(t, "", "paste", f, "--title", "Notes", "--lang", "markdown", "--ttl", "2h")
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "https://paste.example/") {
		t.Fatalf("paste: %d %q %q", code, out, errOut)
	}
	pasteURL := strings.TrimSpace(out)
	pasteID := pasteURL[strings.LastIndex(pasteURL, "/")+1:]
	// paste from stdin
	code, out, _ = runCapture(t, "from stdin\n", "paste")
	if code != 0 || !strings.HasPrefix(out, "https://paste.example/") {
		t.Fatalf("stdin paste: %d %q", code, out)
	}
	// ls shows both
	code, out, errOut = runCapture(t, "", "ls")
	if code != 0 || !strings.Contains(out, "k8s") || !strings.Contains(out, "kubernetes.io") || !strings.Contains(out, pasteID) || !strings.Contains(out, "Notes") {
		t.Fatalf("ls: %d %q %q", code, out, errOut)
	}
	code, out, _ = runCapture(t, "", "ls", "links")
	if code != 0 || strings.Contains(out, pasteID) {
		t.Fatalf("ls links: %d %q", code, out)
	}
	// open prints the public URL for a slug and for a paste id
	code, out, _ = runCapture(t, "", "open", "k8s")
	if code != 0 || strings.TrimSpace(out) != "https://go.example/k8s" {
		t.Fatalf("open link: %d %q", code, out)
	}
	code, out, _ = runCapture(t, "", "open", pasteID)
	if code != 0 || strings.TrimSpace(out) != pasteURL {
		t.Fatalf("open paste: %d %q", code, out)
	}
	// rm auto-detects: link first, then paste
	if code, _, errOut = runCapture(t, "", "rm", "k8s"); code != 0 || !strings.Contains(errOut, "deleted link k8s") {
		t.Fatalf("rm link: %d %q", code, errOut)
	}
	if code, _, errOut = runCapture(t, "", "rm", pasteID); code != 0 || !strings.Contains(errOut, "deleted paste") {
		t.Fatalf("rm paste: %d %q", code, errOut)
	}
	if code, _, errOut = runCapture(t, "", "rm", "nope-nope"); code != 1 || !strings.Contains(errOut, "no link or paste") {
		t.Fatalf("rm missing: %d %q", code, errOut)
	}
	if code, _, errOut = runCapture(t, "", "rm", "--paste", "k8s"); code != 1 || !strings.Contains(errOut, "no such paste") {
		t.Fatalf("rm forced: %d %q", code, errOut)
	}
	// wrong token → clear message, exit 1
	t.Setenv("HOP_TOKEN", "wrong")
	if code, _, errOut = runCapture(t, "", "ls"); code != 1 || !strings.Contains(errOut, "unauthorized") {
		t.Fatalf("bad token: %d %q", code, errOut)
	}
	// no token → tells you to log in, exit 1
	t.Setenv("HOP_TOKEN", "")
	if code, _, errOut = runCapture(t, "", "link", "https://x.example"); code != 1 || !strings.Contains(errOut, "hop login") {
		t.Fatalf("no token: %d %q", code, errOut)
	}
	// usage errors exit 2
	if code, _, _ = runCapture(t, "", "link"); code != 2 {
		t.Fatalf("usage exit: %d", code)
	}
	if code, _, _ = runCapture(t, "", "bogus"); code != 2 {
		t.Fatalf("unknown cmd exit: %d", code)
	}
}
