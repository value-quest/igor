package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"

	"github.com/value-quest/igor/internal/cli"
	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/registry"
	"github.com/value-quest/igor/internal/runner"
)

type fakeRunner struct {
	mu      sync.Mutex
	handler func(name string, args []string) runner.Result
	calls   []string
}

func (f *fakeRunner) IsFake() bool { return true }

func (f *fakeRunner) Run(name string, args ...string) runner.Result {
	return f.RunWith(runner.Opts{}, name, args...)
}

func (f *fakeRunner) RunWith(_ runner.Opts, name string, args ...string) runner.Result {
	cmdLine := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.mu.Lock()
	f.calls = append(f.calls, cmdLine)
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		return h(name, args)
	}
	return runner.Result{}
}

func (f *fakeRunner) LookPath(file string, _ ...string) (string, error) {
	res := f.Run("which", file)
	if !res.OK() {
		return "", fmt.Errorf("not found: %s", file)
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (f *fakeRunner) PortInUse(port string) bool {
	return f.Run("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN").OK()
}

type testEnv struct {
	t      *testing.T
	tmpDir string
	cfg    *config.Config
	fake   *fakeRunner
	app    *cli.App
	out    *bytes.Buffer
	errOut *bytes.Buffer
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmp := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	home := filepath.Join(tmp, "home")
	igorHome := filepath.Join(home, ".igor")
	phpRoot := filepath.Join(home, "frankenphp")
	agents := filepath.Join(home, "Library", "LaunchAgents")
	_ = os.MkdirAll(igorHome, 0o755)
	_ = os.MkdirAll(phpRoot, 0o755)
	_ = os.MkdirAll(agents, 0o755)

	installFakePHP := func(minor, patch string) {
		binDir := filepath.Join(phpRoot, "php"+minor, "bin")
		_ = os.MkdirAll(binDir, 0o755)
		for _, b := range []string{"php", "frankenphp", "composer"} {
			p := filepath.Join(binDir, b)
			_ = os.WriteFile(p, []byte("#!/bin/sh\necho '"+patch+"'\n"), 0o755)
		}
	}
	installFakePHP("8.4", "8.4.28")
	installFakePHP("8.5", "8.5.13")

	cfg := config.New(home, igorHome, filepath.Join(tmp, "igor-bin"), phpRoot, "test", "value-quest/franken-php", "value-quest/igor", agents, false)
	fr := &fakeRunner{}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	app := cli.NewApp(cfg, fr, out, errOut)

	return &testEnv{t: t, tmpDir: tmp, cfg: cfg, fake: fr, app: app, out: out, errOut: errOut}
}

func (e *testEnv) run(args ...string) (string, error) {
	e.out.Reset()
	e.errOut.Reset()
	root := e.app.RootCmd()
	root.SetArgs(args)
	err := root.Execute()
	return e.out.String() + e.errOut.String(), err
}

func (e *testEnv) setupCaddy() {
	if err := e.app.Caddy.EnsureFrontCaddyfile(); err != nil {
		e.t.Fatal(err)
	}
	if err := e.app.Registry.Touch(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) makeProject(name string) string {
	dir := filepath.Join(e.tmpDir, "projects", name)
	_ = os.MkdirAll(filepath.Join(dir, "public"), 0o755)
	return dir
}

func TestUsageNoArgs(t *testing.T) {
	env := newTestEnv(t)
	out, err := env.run()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(out, "Usage: igor <command> [args]") {
		t.Fatalf("expected usage output, got:\n%s", out)
	}
}

func TestAddListInfoRemoveSite(t *testing.T) {
	env := newTestEnv(t)
	env.setupCaddy()
	proj := env.makeProject("portal")
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	_ = os.Chdir(proj)

	env.fake.handler = func(name string, args []string) runner.Result {
		if name == "lsof" || (name == "launchctl" && len(args) > 0 && args[0] == "print") || name == "which" {
			return runner.Result{ExitCode: 1}
		}
		return runner.Result{}
	}

	out, err := env.run("add", "portal.valuequest.test", "--php", "8.4")
	if err != nil {
		t.Fatalf("add failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "https://portal.valuequest.test") {
		t.Fatalf("expected URL in output:\n%s", out)
	}
	if raw, _ := os.ReadFile(filepath.Join(proj, ".php-version")); string(raw) != "8.4\n" {
		t.Fatalf("unexpected .php-version: %q", string(raw))
	}
	if _, err := os.Stat(env.cfg.PlistPath("portal.valuequest~watch")); err != nil {
		t.Fatalf("expected watcher plist: %v", err)
	}

	// Info
	out, err = env.run("info", "portal.valuequest")
	if err != nil || !strings.Contains(out, "http://127.0.0.1:8101") {
		t.Fatalf("info failed: %v\n%s", err, out)
	}

	// List (parallel goroutines)
	out, err = env.run("list")
	if err != nil || !strings.Contains(out, "portal.valuequest") {
		t.Fatalf("list failed: %v\n%s", err, out)
	}

	// Remove
	out, err = env.run("rm", "portal.valuequest")
	if err != nil {
		t.Fatalf("rm failed: %v\n%s", err, out)
	}
	if got, _ := env.app.Registry.Get("portal.valuequest"); got != nil {
		t.Fatalf("expected portal.valuequest removed")
	}
}

func TestServiceAddWithRouteAndEnv(t *testing.T) {
	env := newTestEnv(t)
	env.setupCaddy()

	fakeBin := filepath.Join(env.tmpDir, "fake-mailpit")
	_ = os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755)

	var printCalls int
	env.fake.handler = func(name string, args []string) runner.Result {
		if name == "launchctl" && len(args) > 0 && args[0] == "print" {
			printCalls++
			if printCalls <= 2 {
				return runner.Result{ExitCode: 1}
			}
			return runner.Result{Stdout: "\tstate = running\n"}
		}
		return runner.Result{}
	}

	out, err := env.run("service", "add", "mail", "--route", "8025", "--env", "FOO=bar", "--", fakeBin, "--listen", "8025")
	if err != nil {
		t.Fatalf("service add failed: %v\n%s", err, out)
	}

	plist, _ := os.ReadFile(env.cfg.PlistPath("mail"))
	if !strings.Contains(string(plist), "<key>FOO</key><string>bar</string>") {
		t.Fatalf("missing env in plist:\n%s", string(plist))
	}
	if !strings.Contains(string(plist), "<string>--listen</string>") {
		t.Fatalf("missing args after -- in plist:\n%s", string(plist))
	}
}

func TestRefreshSwitchesPHPAndSyncsWorkers(t *testing.T) {
	env := newTestEnv(t)
	env.setupCaddy()
	proj := env.makeProject("app")
	_ = os.WriteFile(filepath.Join(proj, ".php-version"), []byte("8.5\n"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, "Procfile.dev"), []byte("queue: php artisan queue:listen\n"), 0o644)

	_ = env.app.Registry.Add(registry.Entry{Name: "app", Port: "8101", Type: "php", PHP: "8.4", Dir: proj, Docroot: "public"})

	env.fake.handler = func(name string, args []string) runner.Result {
		if name == "launchctl" && len(args) > 0 && args[0] == "print" {
			return runner.Result{ExitCode: 1}
		}
		return runner.Result{}
	}

	out, err := env.run("refresh", "app")
	if err != nil {
		t.Fatalf("refresh failed: %v\n%s", err, out)
	}
	e, _ := env.app.Registry.Get("app")
	if e == nil || e.PHP != "8.5" {
		t.Fatalf("expected PHP 8.5, got %+v", e)
	}
	if _, err := os.Stat(env.cfg.PlistPath("app@queue")); err != nil {
		t.Fatalf("expected worker plist app@queue: %v", err)
	}
}

func TestUpdateCheckAndSelfUpdateWithHTTPServer(t *testing.T) {
	env := newTestEnv(t)
	arch := goruntime.GOARCH

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/value-quest/franken-php/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.14.0",
				"assets": []map[string]string{
					{"name": "frankenphp-darwin-" + arch + "-php84.tar.gz"},
					{"name": "frankenphp-darwin-" + arch + "-php85.tar.gz"},
					{"name": "versions.json", "browser_download_url": srv.URL + "/versions.json"},
				},
			})
		case "/versions.json":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"frankenphp": "1.14.0",
				"php84":      "8.4.29",
				"php85":      "8.5.13",
			})
		case "/repos/value-quest/igor/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{
					{"name": "igor-darwin-" + arch, "browser_download_url": srv.URL + "/igor-bin"},
				},
			})
		case "/igor-bin":
			_, _ = w.Write([]byte("#!/bin/sh\necho 'igor 9.9.9'\n"))
		}
	}))
	defer srv.Close()

	env.app.Updater.APIBase = srv.URL
	env.app.Updater.GitHubBase = srv.URL
	_ = env.app.Runtimes.MarkerWrite("8.5", "v1.14.0", "1.14.0", "8.5.13")
	_ = env.app.Runtimes.MarkerWrite("8.4", "v1.13.5", "1.13.5", "8.4.28")

	env.fake.handler = func(name string, args []string) runner.Result {
		if strings.Contains(name, "php8.4/bin/php") {
			return runner.Result{Stdout: "8.4.28"}
		}
		if strings.Contains(name, "php8.5/bin/php") {
			return runner.Result{Stdout: "8.5.13"}
		}
		return runner.Result{}
	}

	out, err := env.run("update", "--check")
	if err != nil {
		t.Fatalf("update --check failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Run 'igor update' to install PHP 8.4.") {
		t.Fatalf("unexpected update --check output:\n%s", out)
	}

	out, err = env.run("self-update")
	if err != nil {
		t.Fatalf("self-update failed: %v\n%s", err, out)
	}
	if raw, err := os.ReadFile(env.cfg.IgorBin); err != nil || !strings.Contains(string(raw), "igor 9.9.9") {
		t.Fatalf("expected replaced binary at %s, err=%v", env.cfg.IgorBin, err)
	}
}

func TestDoctorAndUninstall(t *testing.T) {
	env := newTestEnv(t)
	env.setupCaddy()
	_, _ = env.app.Shims.CreateShims()

	env.fake.handler = func(name string, args []string) runner.Result {
		if name == "which" {
			return runner.Result{Stdout: "/opt/homebrew/bin/" + args[0]}
		}
		if name == "pgrep" {
			return runner.Result{ExitCode: 1}
		}
		if name == "launchctl" && len(args) > 0 && args[0] == "print" {
			return runner.Result{ExitCode: 1}
		}
		return runner.Result{}
	}

	out, _ := env.run("doctor")
	if !strings.Contains(out, "Homebrew") || !strings.Contains(out, "PHP runtimes") {
		t.Fatalf("unexpected doctor output:\n%s", out)
	}

	out, err := env.run("uninstall", "8.4", "--yes")
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(env.cfg.PHPRoot, "php8.4")); !os.IsNotExist(err) {
		t.Fatalf("expected php8.4 removed")
	}
}
