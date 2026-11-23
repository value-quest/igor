//go:build darwin

package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/value-quest/igor/internal/cli"
	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/runner"
)

// extractMainBashScript returns a path to the bash `igor` script from git branch `main`.
// Skips the test if git `main:igor` is unavailable (e.g. shallow CI checkout without main).
func extractMainBashScript(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "show", "main:igor")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		t.Skipf("skipping bash differential test: git show main:igor unavailable (%v)", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "igor-bash")
	if err := os.WriteFile(scriptPath, out, 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath
}

type stubbedEnv struct {
	root       string
	home       string
	igorHome   string
	phpRoot    string
	agentsDir  string
	fakeBinDir string
	fakeSvcBin string
	portalDir  string
	blogDir    string
}

func makeStubbedEnv(t *testing.T, label string) *stubbedEnv {
	t.Helper()
	root := filepath.Join(t.TempDir(), label)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	home := filepath.Join(root, "home")
	igorHome := filepath.Join(home, ".igor")
	phpRoot := filepath.Join(home, "frankenphp")
	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	fakeBinDir := filepath.Join(root, "fakebin")
	portalDir := filepath.Join(root, "projects", "portal")
	blogDir := filepath.Join(root, "projects", "blog")

	for _, d := range []string{igorHome, filepath.Join(igorHome, "sites"), filepath.Join(igorHome, "logs"), phpRoot, agentsDir, fakeBinDir, filepath.Join(portalDir, "public"), blogDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Pre-create Caddyfile + empty sites.tsv (simulating `igor setup` already ran)
	caddyfile := "{\n\tlocal_certs\n\tadmin localhost:2019\n}\n\nimport " + filepath.Join(igorHome, "sites") + "/*.caddy\n"
	_ = os.WriteFile(filepath.Join(igorHome, "Caddyfile"), []byte(caddyfile), 0o644)
	_ = os.WriteFile(filepath.Join(igorHome, "sites.tsv"), nil, 0o644)

	// Fake PHP 8.4 and 8.5 runtimes
	for _, pair := range [][2]string{{"8.4", "8.4.28"}, {"8.5", "8.5.13"}} {
		binDir := filepath.Join(phpRoot, "php"+pair[0], "bin")
		_ = os.MkdirAll(binDir, 0o755)
		for _, tool := range []string{"php", "frankenphp", "composer"} {
			_ = os.WriteFile(filepath.Join(binDir, tool), []byte("#!/bin/sh\necho '"+pair[1]+"'\n"), 0o755)
		}
	}

	// Procfile.dev for portal
	_ = os.WriteFile(filepath.Join(portalDir, "Procfile.dev"), []byte("# workers\nqueue: php artisan queue:listen --tries=3\nreverb: php artisan reverb:start\n"), 0o644)

	// Fake service binary
	fakeSvcBin := filepath.Join(root, "fake-mailpit")
	_ = os.WriteFile(fakeSvcBin, []byte("#!/bin/sh\nexit 0\n"), 0o755)

	// Stateful stubbed launchctl (tracks bootstrapped agents in root/launchd-state)
	stateDir := filepath.Join(root, "launchd-state")
	_ = os.MkdirAll(stateDir, 0o755)
	_ = os.WriteFile(filepath.Join(fakeBinDir, "launchctl"), []byte(`#!/bin/sh
STATE="`+stateDir+`"
case "$1" in
  bootstrap)
    label=$(basename "$3" .plist)
    touch "$STATE/$label"
    exit 0 ;;
  bootout)
    label=${2##*/}
    rm -f "$STATE/$label"
    exit 0 ;;
  print)
    label=${2##*/}
    if [ -f "$STATE/$label" ]; then
      printf '\tstate = running\n'
      exit 0
    fi
    exit 1 ;;
  *) exit 0 ;;
esac
`), 0o755)
	_ = os.WriteFile(filepath.Join(fakeBinDir, "lsof"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	_ = os.WriteFile(filepath.Join(fakeBinDir, "direnv"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	_ = os.WriteFile(filepath.Join(fakeBinDir, "osascript"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	_ = os.WriteFile(filepath.Join(fakeBinDir, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755)

	return &stubbedEnv{
		root:       root,
		home:       home,
		igorHome:   igorHome,
		phpRoot:    phpRoot,
		agentsDir:  agentsDir,
		fakeBinDir: fakeBinDir,
		fakeSvcBin: fakeSvcBin,
		portalDir:  portalDir,
		blogDir:    blogDir,
	}
}

func (s *stubbedEnv) runBash(t *testing.T, bashScript, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/bin/bash", append([]string{bashScript}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + s.home,
		"IGOR_HOME=" + s.igorHome,
		"IGOR_PHP_ROOT=" + s.phpRoot,
		"IGOR_TLD=test",
		"PATH=" + s.fakeBinDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash igor %v failed: %v\n%s", args, err, string(out))
	}
	return string(out)
}

func normalizeContent(content, root, binPath string) string {
	s := strings.ReplaceAll(content, binPath, "<IGOR_BIN>")
	s = strings.ReplaceAll(s, root, "<ROOT>")
	return s
}

func TestCompatByteForByteAgainstMainBash(t *testing.T) {
	bashScript := extractMainBashScript(t)
	bashEnv := makeStubbedEnv(t, "bash")
	goEnv := makeStubbedEnv(t, "go")

	// 1. Run identical commands on main's bash script
	bashEnv.runBash(t, bashScript, bashEnv.home, "shims")
	bashEnv.runBash(t, bashScript, bashEnv.portalDir, "add", "portal.valuequest.test", "--php", "8.4")
	bashEnv.runBash(t, bashScript, bashEnv.blogDir, "add", "blog", "--docroot", ".", "--php", "8.5")
	bashEnv.runBash(t, bashScript, bashEnv.home, "proxy", "reverb", "8080")
	bashEnv.runBash(t, bashScript, bashEnv.home, "service", "add", "mail", "--route", "8025", "--env", "FOO=bar", "--env", "BAZ=qux", "--", bashEnv.fakeSvcBin, "--listen", "8025")

	// 2. Run identical commands on Go igor
	goBin := filepath.Join(goEnv.root, "igor-go")
	cfg := config.New(goEnv.home, goEnv.igorHome, goBin, goEnv.phpRoot, "test", "value-quest/franken-php", "value-quest/igor", goEnv.agentsDir, false)
	goRunning := map[string]bool{}
	fr := &fakeRunner{
		handler: func(name string, args []string) runner.Result {
			if name == "lsof" {
				return runner.Result{ExitCode: 1}
			}
			if name == "launchctl" && len(args) >= 2 {
				switch args[0] {
				case "bootstrap":
					if len(args) >= 3 {
						label := strings.TrimSuffix(filepath.Base(args[2]), ".plist")
						goRunning[label] = true
					}
					return runner.Result{}
				case "bootout":
					if idx := strings.LastIndexByte(args[1], '/'); idx >= 0 {
						delete(goRunning, args[1][idx+1:])
					}
					return runner.Result{}
				case "print":
					if idx := strings.LastIndexByte(args[1], '/'); idx >= 0 && goRunning[args[1][idx+1:]] {
						return runner.Result{Stdout: "\tstate = running\n"}
					}
					return runner.Result{ExitCode: 1}
				}
			}
			return runner.Result{}
		},
	}
	var outBuf, errBuf bytes.Buffer
	app := cli.NewApp(cfg, fr, &outBuf, &errBuf)

	runGo := func(dir string, args ...string) {
		t.Helper()
		origWd, _ := os.Getwd()
		defer func() { _ = os.Chdir(origWd) }()
		_ = os.Chdir(dir)
		outBuf.Reset()
		errBuf.Reset()
		root := app.RootCmd()
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("go igor %v failed: %v\n%s%s", args, err, outBuf.String(), errBuf.String())
		}
	}

	runGo(goEnv.home, "shims")
	runGo(goEnv.portalDir, "add", "portal.valuequest.test", "--php", "8.4")
	runGo(goEnv.blogDir, "add", "blog", "--docroot", ".", "--php", "8.5")
	runGo(goEnv.home, "proxy", "reverb", "8080")
	runGo(goEnv.home, "service", "add", "mail", "--route", "8025", "--env", "FOO=bar", "--env", "BAZ=qux", "--", goEnv.fakeSvcBin, "--listen", "8025")

	// 3. Compare every generated file byte-for-byte!
	filesToCompare := []string{
		"home/.igor/sites.tsv",
		"home/.igor/sites/portal.valuequest.caddy",
		"home/.igor/sites/blog.caddy",
		"home/.igor/sites/reverb.caddy",
		"home/.igor/sites/mail.caddy",
		"home/.igor/backends/portal.valuequest.Caddyfile",
		"home/.igor/backends/blog.Caddyfile",
		"home/.igor/bin/igor-shim",
		"home/.igor/bin/igor-worker",
		"home/Library/LaunchAgents/dev.igor.portal.valuequest.plist",
		"home/Library/LaunchAgents/dev.igor.portal.valuequest@queue.plist",
		"home/Library/LaunchAgents/dev.igor.portal.valuequest@reverb.plist",
		"home/Library/LaunchAgents/dev.igor.portal.valuequest~watch.plist",
		"home/Library/LaunchAgents/dev.igor.blog.plist",
		"home/Library/LaunchAgents/dev.igor.blog~watch.plist",
		"home/Library/LaunchAgents/dev.igor.mail.plist",
		"projects/portal/.php-version",
		"projects/portal/.envrc",
		"projects/blog/.php-version",
		"projects/blog/.envrc",
	}

	for _, rel := range filesToCompare {
		bashBytes, err := os.ReadFile(filepath.Join(bashEnv.root, rel))
		if err != nil {
			t.Fatalf("bash missing %s: %v", rel, err)
		}
		goBytes, err := os.ReadFile(filepath.Join(goEnv.root, rel))
		if err != nil {
			t.Fatalf("go missing %s: %v", rel, err)
		}

		want := normalizeContent(string(bashBytes), bashEnv.root, bashScript)
		got := normalizeContent(string(goBytes), goEnv.root, goBin)
		if want != got {
			t.Errorf("byte mismatch in %s:\n--- bash (main) ---\n%s\n--- go-rewrite ---\n%s", rel, want, got)
		}
	}

	// 4. Compare shim directory symlink set
	listDir := func(dir string) []string {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		return names
	}
	bashShims := strings.Join(listDir(filepath.Join(bashEnv.igorHome, "bin")), ",")
	goShims := strings.Join(listDir(filepath.Join(goEnv.igorHome, "bin")), ",")
	if bashShims != goShims {
		t.Errorf("shim dir mismatch:\nbash: %s\ngo:   %s", bashShims, goShims)
	}
}

func TestCompatInPlaceUpgradeFromMainBash(t *testing.T) {
	bashScript := extractMainBashScript(t)
	env := makeStubbedEnv(t, "upgrade")

	// Populate state using main's bash script
	env.runBash(t, bashScript, env.home, "shims")
	env.runBash(t, bashScript, env.portalDir, "add", "portal.valuequest", "--php", "8.4")
	env.runBash(t, bashScript, env.home, "proxy", "reverb", "8080")
	env.runBash(t, bashScript, env.home, "service", "add", "mail", "--route", "8025", "--env", "FOO=bar", "--", env.fakeSvcBin)

	// Also append a legacy 5-column PHP row (no docroot column) as older igor versions wrote
	f, err := os.OpenFile(filepath.Join(env.igorHome, "sites.tsv"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("legacy\t8102\tphp\t8.5\t" + env.blogDir + "\n")
	f.Close()
	_ = os.WriteFile(filepath.Join(env.blogDir, ".php-version"), []byte("8.5\n"), 0o644)

	// Record mtime + bytes of bash-created plists and backend Caddyfiles
	unchangedFiles := []string{
		filepath.Join(env.agentsDir, "dev.igor.portal.valuequest.plist"),
		filepath.Join(env.agentsDir, "dev.igor.portal.valuequest@queue.plist"),
		filepath.Join(env.agentsDir, "dev.igor.portal.valuequest@reverb.plist"),
		filepath.Join(env.agentsDir, "dev.igor.mail.plist"),
		filepath.Join(env.igorHome, "backends", "portal.valuequest.Caddyfile"),
	}
	beforeTime := time.Now().Add(-2 * time.Second)
	origBytes := map[string]string{}
	for _, p := range unchangedFiles {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing bash file %s: %v", p, err)
		}
		origBytes[p] = string(raw)
		_ = os.Chtimes(p, beforeTime, beforeTime)
	}

	// Now switch to Go igor on that EXACT bash-created ~/.igor directory
	cfg := config.New(env.home, env.igorHome, bashScript, env.phpRoot, "test", "value-quest/franken-php", "value-quest/igor", env.agentsDir, false)
	fr := &fakeRunner{
		handler: func(name string, args []string) runner.Result {
			if name == "lsof" {
				return runner.Result{ExitCode: 1}
			}
			if name == "launchctl" && len(args) > 0 && args[0] == "print" {
				return runner.Result{ExitCode: 1}
			}
			return runner.Result{}
		},
	}
	var outBuf, errBuf bytes.Buffer
	app := cli.NewApp(cfg, fr, &outBuf, &errBuf)

	runGo := func(args ...string) string {
		t.Helper()
		outBuf.Reset()
		errBuf.Reset()
		root := app.RootCmd()
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("go igor %v failed: %v\n%s%s", args, err, outBuf.String(), errBuf.String())
		}
		return outBuf.String() + errBuf.String()
	}

	// 1. `igor ls` (alias) must read all bash-created entries + workers
	listOut := runGo("ls")
	for _, want := range []string{"portal.valuequest", ":queue", ":reverb", "reverb", "mail", "legacy"} {
		if !strings.Contains(listOut, want) {
			t.Errorf("go igor ls missing %q in:\n%s", want, listOut)
		}
	}

	// 2. `igor refresh` must NOT touch unchanged bash-created plists/backends (no macOS Background Activity banners!)
	runGo("refresh")
	for _, p := range unchangedFiles {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("file vanished after refresh: %s", p)
		}
		if !st.ModTime().Equal(beforeTime) {
			afterRaw, _ := os.ReadFile(p)
			t.Errorf("refresh rewrote unchanged bash file %s!\n--- before ---\n%s\n--- after ---\n%s", p, origBytes[p], string(afterRaw))
		}
	}

	// 3. `igor rm portal.valuequest` cleanly removes site, workers, watcher, route, backend
	runGo("rm", "portal.valuequest")
	for _, gone := range []string{
		filepath.Join(env.agentsDir, "dev.igor.portal.valuequest.plist"),
		filepath.Join(env.agentsDir, "dev.igor.portal.valuequest@queue.plist"),
		filepath.Join(env.agentsDir, "dev.igor.portal.valuequest@reverb.plist"),
		filepath.Join(env.agentsDir, "dev.igor.portal.valuequest~watch.plist"),
		filepath.Join(env.igorHome, "sites", "portal.valuequest.caddy"),
		filepath.Join(env.igorHome, "backends", "portal.valuequest.Caddyfile"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed by igor rm", gone)
		}
	}
	// Project files untouched
	if _, err := os.Stat(filepath.Join(env.portalDir, ".php-version")); err != nil {
		t.Errorf("igor rm should not delete .php-version")
	}
}

func TestCompatCLIErrorContracts(t *testing.T) {
	env := newTestEnv(t)
	env.setupCaddy()
	proj := env.makeProject("errtest")
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	_ = os.Chdir(proj)

	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"add", "proxy"}, "reserved"},
		{[]string{"add", "Bad_Name"}, "invalid name"},
		{[]string{"add", "ok", "--docroot", "../escape"}, "--docroot must be a folder inside the project"},
		{[]string{"proxy", "reverb", "notaport"}, "port must be a number"},
		{[]string{"service", "add", "bad", "--env", "NOEQUALS", "--", "/bin/sh"}, "--env needs KEY=VALUE"},
		{[]string{"rm", "nonexistent"}, "no site, service or route"},
		{[]string{"logs", "--clear", "portal:watch"}, "no log for 'portal:watch'"},
	}

	for _, tc := range cases {
		_, err := env.run(tc.args...)
		if err == nil {
			t.Errorf("igor %v: expected error containing %q, got nil", tc.args, tc.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("igor %v: expected error containing %q, got %q", tc.args, tc.wantErr, err.Error())
		}
	}
}
