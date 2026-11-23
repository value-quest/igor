package supervisor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/runner"
	"github.com/value-quest/igor/internal/supervisor"
)

type fakeRun struct {
	active map[string]bool
	calls  []string
}

func (f *fakeRun) IsFake() bool { return true }
func (f *fakeRun) Run(name string, args ...string) runner.Result {
	return f.RunWith(runner.Opts{}, name, args...)
}
func (f *fakeRun) RunWith(_ runner.Opts, name string, args ...string) runner.Result {
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	if name == "systemctl" && len(args) >= 3 && args[0] == "--user" {
		switch args[1] {
		case "enable":
			if len(args) >= 4 && args[2] == "--now" {
				f.active[args[3]] = true
			}
		case "disable":
			if len(args) >= 4 && args[2] == "--now" {
				delete(f.active, args[3])
			}
		case "stop":
			delete(f.active, args[2])
		case "restart":
			f.active[args[2]] = true
		case "is-active":
			unit := args[len(args)-1]
			if f.active[unit] {
				return runner.Result{}
			}
			return runner.Result{ExitCode: 3}
		}
	}
	return runner.Result{}
}
func (f *fakeRun) LookPath(string, ...string) (string, error) { return "", nil }
func (f *fakeRun) PortInUse(string) bool                      { return false }

func TestSystemdServiceWorkerAndWatcherLifecycle(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.New(tmp, filepath.Join(tmp, ".igor"), filepath.Join(tmp, "igor"), filepath.Join(tmp, "frankenphp"), "test", "value-quest/franken-php", "value-quest/igor", filepath.Join(tmp, "agents"), false)
	fr := &fakeRun{active: map[string]bool{}}
	mgr := supervisor.NewSystemd(cfg, fr)

	// 1. Write a site service unit
	changed, err := mgr.WriteUnit("portal.valuequest", "/home/user/portal", "/usr/bin:/bin", []string{"/home/user/frankenphp/php8.4/bin/frankenphp", "run", "--config", "/tmp/portal.Caddyfile"}, []supervisor.EnvVar{{Key: "APP_ENV", Value: "local"}}, nil)
	if err != nil || !changed {
		t.Fatalf("expected changed=true, err=nil; got changed=%v err=%v", changed, err)
	}
	// Writing identical content should be idempotent
	changed, err = mgr.WriteUnit("portal.valuequest", "/home/user/portal", "/usr/bin:/bin", []string{"/home/user/frankenphp/php8.4/bin/frankenphp", "run", "--config", "/tmp/portal.Caddyfile"}, []supervisor.EnvVar{{Key: "APP_ENV", Value: "local"}}, nil)
	if err != nil || changed {
		t.Fatalf("expected idempotent write changed=false, got changed=%v err=%v", changed, err)
	}

	raw, err := os.ReadFile(mgr.UnitPath("portal.valuequest"))
	if err != nil {
		t.Fatalf("expected unit file at %s: %v", mgr.UnitPath("portal.valuequest"), err)
	}
	content := string(raw)
	for _, want := range []string{
		"WorkingDirectory=/home/user/portal",
		`Environment="APP_ENV=local"`,
		"Restart=always",
		"StandardOutput=append:" + cfg.LogPath("portal.valuequest"),
		"WantedBy=default.target",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("unit missing %q:\n%s", want, content)
		}
	}

	if cmd := mgr.CommandString("portal.valuequest"); !strings.Contains(cmd, "frankenphp run --config /tmp/portal.Caddyfile") {
		t.Fatalf("unexpected CommandString: %q", cmd)
	}

	// Start / IsRunning / Stop
	if err := mgr.Start("portal.valuequest"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if !mgr.IsRunning("portal.valuequest") {
		t.Fatalf("expected portal.valuequest running")
	}
	if err := mgr.Stop("portal.valuequest"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if mgr.IsRunning("portal.valuequest") {
		t.Fatalf("expected portal.valuequest stopped")
	}

	// 2. Worker unit (ensures '@' is NOT left in systemd unit filename)
	if _, err := mgr.WriteUnit("portal.valuequest@queue", "/home/user/portal", "/usr/bin:/bin", []string{"/home/user/.igor/bin/igor-worker", "php artisan queue:listen"}, nil, nil); err != nil {
		t.Fatalf("WriteUnit worker failed: %v", err)
	}
	if strings.Contains(filepath.Base(mgr.UnitPath("portal.valuequest@queue")), "@") {
		t.Fatalf("systemd unit filename must not contain '@': %s", mgr.UnitPath("portal.valuequest@queue"))
	}
	workers := mgr.SiteWorkers("portal.valuequest")
	if len(workers) != 1 || workers[0] != "portal.valuequest@queue" {
		t.Fatalf("unexpected SiteWorkers: %v", workers)
	}

	// 3. Watcher unit (.service + .path)
	if _, err := mgr.WriteUnit("portal.valuequest~watch", "/home/user/portal", "/usr/bin:/bin", []string{"/usr/local/bin/igor", "refresh", "portal.valuequest"}, []supervisor.EnvVar{{Key: "IGOR_WATCH", Value: "1"}}, []string{"/home/user/portal/.php-version", "/home/user/portal/Procfile.dev"}); err != nil {
		t.Fatalf("WriteUnit watcher failed: %v", err)
	}
	pathUnit := mgr.UnitPath("portal.valuequest~watch")
	if !strings.HasSuffix(pathUnit, ".path") {
		t.Fatalf("expected watcher primary unit to end with .path, got %s", pathUnit)
	}
	pathRaw, err := os.ReadFile(pathUnit)
	if err != nil {
		t.Fatalf("expected .path unit: %v", err)
	}
	if !strings.Contains(string(pathRaw), "PathChanged=/home/user/portal/.php-version") || !strings.Contains(string(pathRaw), "Unit=dev.igor.portal.valuequest--watch.service") {
		t.Fatalf("unexpected .path content:\n%s", string(pathRaw))
	}

	// Remove deletes both .path and .service
	if err := mgr.Remove("portal.valuequest~watch"); err != nil {
		t.Fatalf("Remove watcher failed: %v", err)
	}
	if mgr.Exists("portal.valuequest~watch") {
		t.Fatalf("expected watcher removed")
	}
}
