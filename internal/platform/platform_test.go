package platform_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/platform"
	"github.com/value-quest/igor/internal/registry"
	"github.com/value-quest/igor/internal/runner"
	"github.com/value-quest/igor/internal/ui"
)

type stubRunner struct {
	handler func(name string, args []string) runner.Result
}

func (s *stubRunner) IsFake() bool { return true }
func (s *stubRunner) Run(name string, args ...string) runner.Result {
	return s.RunWith(runner.Opts{}, name, args...)
}
func (s *stubRunner) RunWith(_ runner.Opts, name string, args ...string) runner.Result {
	if s.handler != nil {
		return s.handler(name, args)
	}
	return runner.Result{}
}
func (s *stubRunner) LookPath(file string, _ ...string) (string, error) {
	return "/usr/bin/" + file, nil
}
func (s *stubRunner) PortInUse(string) bool { return false }

func TestWSL2SyncHosts(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.New(tmp, filepath.Join(tmp, ".igor"), filepath.Join(tmp, "igor"), filepath.Join(tmp, "frankenphp"), "test", "value-quest/franken-php", "value-quest/igor", filepath.Join(tmp, "agents"), false)
	hostsFile := filepath.Join(tmp, "hosts")
	_ = os.WriteFile(hostsFile, []byte("127.0.0.1 localhost\n"), 0o644)
	cfg.WindowsHostsPath = hostsFile

	reg := registry.New(cfg)
	_ = reg.Touch()
	_ = reg.Add(registry.Entry{Name: "portal", Port: "8101", Type: "php", PHP: "8.4", Dir: tmp, Docroot: "public"})
	_ = reg.Add(registry.Entry{Name: "redis", Port: "-", Type: "service", PHP: "-", Dir: tmp})
	_ = reg.Add(registry.Entry{Name: "mail", Port: "8025", Type: "service", PHP: "-", Dir: tmp})

	var out, errOut bytes.Buffer
	plat := platform.New(platform.WSL2, cfg, &stubRunner{}, ui.New(&out, &errOut, false), reg)

	if err := plat.SyncHosts(); err != nil {
		t.Fatalf("SyncHosts failed: %v", err)
	}
	raw, _ := os.ReadFile(hostsFile)
	got := string(raw)
	if !strings.Contains(got, "127.0.0.1 localhost") {
		t.Fatalf("existing hosts entry lost:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1 portal.test\n") || !strings.Contains(got, "127.0.0.1 mail.test\n") {
		t.Fatalf("missing synced domains:\n%s", got)
	}
	if strings.Contains(got, "redis.test") {
		t.Fatalf("unrouted service should not be in hosts:\n%s", got)
	}

	// Removing sites should clean up the managed block
	_ = reg.Remove("portal")
	_ = reg.Remove("mail")
	if err := plat.SyncHosts(); err != nil {
		t.Fatalf("SyncHosts cleanup failed: %v", err)
	}
	raw, _ = os.ReadFile(hostsFile)
	if strings.Contains(string(raw), "# igor-start") || strings.Contains(string(raw), "portal.test") {
		t.Fatalf("expected igor block removed:\n%s", string(raw))
	}
}

func TestLinuxDoctorAndCapabilities(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.New(tmp, filepath.Join(tmp, ".igor"), filepath.Join(tmp, "igor"), filepath.Join(tmp, "frankenphp"), "test", "value-quest/franken-php", "value-quest/igor", filepath.Join(tmp, "agents"), false)
	reg := registry.New(cfg)
	var out, errOut bytes.Buffer

	var setcapCalled bool
	sr := &stubRunner{
		handler: func(name string, args []string) runner.Result {
			if name == "getcap" {
				return runner.Result{Stdout: ""}
			}
			if name == "sudo" && len(args) > 0 && args[0] == "setcap" {
				setcapCalled = true
				return runner.Result{}
			}
			return runner.Result{}
		},
	}

	plat := platform.New(platform.Linux, cfg, sr, ui.New(&out, &errOut, false), reg)
	if err := plat.EnsureProxyCapabilities("/home/user/frankenphp/php8.5/bin/frankenphp"); err != nil {
		t.Fatalf("EnsureProxyCapabilities failed: %v", err)
	}
	if !setcapCalled {
		t.Fatalf("expected sudo setcap to be called when capability missing")
	}

	rows, failures, _ := plat.DoctorEnvChecks("/home/user/frankenphp/php8.5/bin/frankenphp")
	if failures != 0 {
		t.Fatalf("expected 0 failures, got %d; rows=%v", failures, rows)
	}
}
