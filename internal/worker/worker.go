package worker

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/launchd"
	"github.com/value-quest/igor/internal/registry"
)

type Manager struct {
	cfg     *config.Config
	launchd *launchd.Manager
}

func New(cfg *config.Config, ld *launchd.Manager) *Manager {
	return &Manager{cfg: cfg, launchd: ld}
}

func (m *Manager) SiteWorkers(site string) []string {
	pattern := filepath.Join(m.cfg.AgentsDir, m.cfg.Label(site)+"@*.plist")
	files, _ := filepath.Glob(pattern)
	sort.Strings(files)

	prefix := config.LabelPrefix + "."
	var out []string
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".plist")
		if strings.HasPrefix(base, prefix) {
			out = append(out, strings.TrimPrefix(base, prefix))
		}
	}
	return out
}

func (m *Manager) WriteWorkerRunner() (string, error) {
	if err := m.cfg.EnsureDir(m.cfg.ShimDir); err != nil {
		return "", err
	}
	file := filepath.Join(m.cfg.ShimDir, "igor-worker")
	content := `#!/bin/bash
# igor: runs one Procfile.dev line using the site's current PHP version
set -eu
if [ "${1:-}" = "--site" ]; then
    site="$2"
    shift 2
    IGOR_HOME="${IGOR_HOME:-$HOME/.igor}"
    PHP_ROOT="${IGOR_PHP_ROOT:-$HOME/frankenphp}"
    php=$(awk -F'\t' -v n="$site" '$1==n {print $4; exit}' "$IGOR_HOME/sites.tsv")
    if [ -n "$php" ]; then
        export PATH="$PHP_ROOT/php$php/bin:$PATH"
    fi
fi
exec /bin/bash -c "$1"
`
	if existing, err := os.ReadFile(file); err != nil || string(existing) != content {
		tmp := file + ".tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o755); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, file); err != nil {
			return "", err
		}
	}
	return file, nil
}

type ProcEntry struct {
	Name string
	Cmd  string
}

var procLineRe = regexp.MustCompile(`^([A-Za-z0-9_-]+):\s*(.+)$`)

func ParseProcfile(path string) ([]ProcEntry, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []ProcEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		sm := procLineRe.FindStringSubmatch(line)
		if sm == nil {
			return nil, fmt.Errorf("Procfile.dev: can't read '%s' (expected 'name: command')", line)
		}
		if sm[1] == "watch" {
			return nil, fmt.Errorf("Procfile.dev: 'watch' is reserved for igor's watcher - rename that worker")
		}
		entries = append(entries, ProcEntry{Name: sm[1], Cmd: sm[2]})
	}
	return entries, sc.Err()
}

func (m *Manager) SyncWorkers(entry registry.Entry, keepStopped bool) ([]string, error) {
	procfile := filepath.Join(entry.Dir, "Procfile.dev")
	keep := map[string]bool{}
	var changes []string

	if _, err := os.Stat(procfile); err == nil {
		runnerPath, err := m.WriteWorkerRunner()
		if err != nil {
			return nil, err
		}
		defined, err := ParseProcfile(procfile)
		if err != nil {
			return nil, err
		}

		for _, w := range defined {
			agent := entry.Name + "@" + w.Name
			keep[agent] = true

			_, statErr := os.Stat(m.cfg.PlistPath(agent))
			existed := statErr == nil
			running := m.launchd.IsRunning(agent)

			path := strings.Join([]string{
				filepath.Join(entry.Dir, "vendor", "bin"),
				filepath.Join(entry.Dir, "node_modules", ".bin"),
				"/opt/homebrew/bin",
				"/usr/local/bin",
				"/usr/bin",
				"/bin",
				"/usr/sbin",
				"/sbin",
			}, ":")

			changed, err := m.launchd.WritePlist(
				agent,
				entry.Dir,
				path,
				[]string{runnerPath, "--site", entry.Name, w.Cmd},
				[]launchd.EnvVar{
					{Key: "IGOR_HOME", Value: m.cfg.IgorHome},
					{Key: "IGOR_PHP_ROOT", Value: m.cfg.PHPRoot},
				},
				nil,
			)
			if err != nil {
				return nil, err
			}
			if changed {
				if err := m.launchd.Stop(agent); err != nil {
					return nil, err
				}
				changes = append(changes, fmt.Sprintf("%s:%s  %s", entry.Name, w.Name, w.Cmd))
			}
			if !keepStopped || !existed || running {
				if err := m.launchd.Start(agent); err != nil {
					return nil, err
				}
			}
		}
	}

	for _, agent := range m.SiteWorkers(entry.Name) {
		if !keep[agent] {
			if err := m.launchd.Remove(agent); err != nil {
				return nil, err
			}
			short := agent[strings.Index(agent, "@")+1:]
			changes = append(changes, fmt.Sprintf("%s:%s  removed", entry.Name, short))
		}
	}

	return changes, nil
}

// EnsureGlobalWatcher creates/starts the single global `dev.igor.watch` LaunchAgent
// that watches all registered sites (avoiding per-site watcher plists and macOS banner spam).
func (m *Manager) EnsureGlobalWatcher() error {
	const agent = "watch"
	_, statErr := os.Stat(m.cfg.PlistPath(agent))
	existed := statErr == nil
	running := m.launchd.IsRunning(agent)

	watchPathEnv := strings.Join([]string{
		filepath.Join(m.cfg.Home, ".nvm", "current", "bin"),
		filepath.Join(m.cfg.Home, ".volta", "bin"),
		filepath.Join(m.cfg.Home, ".fnm", "current", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}, ":")

	changed, err := m.launchd.WritePlist(
		agent,
		m.cfg.IgorHome,
		watchPathEnv,
		[]string{m.cfg.IgorBin, "watch"},
		[]launchd.EnvVar{
			{Key: "IGOR_WATCH", Value: "1"},
			{Key: "IGOR_HOME", Value: m.cfg.IgorHome},
			{Key: "IGOR_PHP_ROOT", Value: m.cfg.PHPRoot},
		},
		nil,
	)
	if err != nil {
		return err
	}
	if !existed || !running {
		return m.launchd.Start(agent)
	}
	if changed {
		if err := m.launchd.Stop(agent); err != nil {
			return err
		}
		return m.launchd.Start(agent)
	}
	return nil
}

// CleanupLegacyWatchers removes old per-site `dev.igor.<site>~watch.plist` LaunchAgents.
func (m *Manager) CleanupLegacyWatchers() {
	pattern := filepath.Join(m.cfg.AgentsDir, config.LabelPrefix+".*~watch.plist")
	files, _ := filepath.Glob(pattern)
	prefix := config.LabelPrefix + "."
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".plist")
		if strings.HasPrefix(base, prefix) {
			agent := strings.TrimPrefix(base, prefix)
			_ = m.launchd.Remove(agent)
		}
	}
}

// WriteWatcher seeds the site's branch state and ensures the global watcher is running.
func (m *Manager) WriteWatcher(entry registry.Entry) error {
	SeedBranchState(m.cfg, entry.Name, entry.Dir)
	return m.EnsureGlobalWatcher()
}
