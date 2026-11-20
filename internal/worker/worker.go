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
	content := "#!/bin/bash\n# igor: runs one Procfile.dev line\nexec /bin/bash -c \"$1\"\n"
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
				m.cfg.PHPBin(entry.PHP),
				filepath.Join(entry.Dir, "vendor", "bin"),
				filepath.Join(entry.Dir, "node_modules", ".bin"),
				"/opt/homebrew/bin",
				"/usr/local/bin",
				"/usr/bin",
				"/bin",
				"/usr/sbin",
				"/sbin",
			}, ":")

			changed, err := m.launchd.WritePlist(agent, entry.Dir, path, []string{runnerPath, w.Cmd}, nil, nil)
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

func (m *Manager) WriteWatcher(entry registry.Entry) error {
	w := m.cfg.WatcherName(entry.Name)
	_, statErr := os.Stat(m.cfg.PlistPath(w))
	existed := statErr == nil
	running := m.launchd.IsRunning(w)

	changed, err := m.launchd.WritePlist(
		w,
		entry.Dir,
		"/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin",
		[]string{m.cfg.IgorBin, "refresh", entry.Name},
		[]launchd.EnvVar{{Key: "IGOR_WATCH", Value: "1"}},
		[]string{
			filepath.Join(entry.Dir, ".php-version"),
			filepath.Join(entry.Dir, "composer.json"),
			filepath.Join(entry.Dir, "Procfile.dev"),
		},
	)
	if err != nil {
		return err
	}
	if !existed {
		return m.launchd.Start(w)
	}
	if changed && running {
		if err := m.launchd.Stop(w); err != nil {
			return err
		}
		return m.launchd.Start(w)
	}
	return nil
}
