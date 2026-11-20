package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/runner"
)

type Manager struct {
	cfg *config.Config
	run runner.Runner
}

func New(cfg *config.Config, run runner.Runner) *Manager {
	return &Manager{cfg: cfg, run: run}
}

var (
	dirVerRe      = regexp.MustCompile(`^php(\d+)\.(\d+)$`)
	phpVerStartRe = regexp.MustCompile(`^(\d+\.\d+)`)
	platformRe    = regexp.MustCompile(`^(\d+\.\d+)(\.\d+)?$`)
)

// InstalledVersions returns installed PHP minor versions, newest first (e.g. ["8.5", "8.4"]).
func (m *Manager) InstalledVersions() []string {
	entries, err := os.ReadDir(m.cfg.PHPRoot)
	if err != nil {
		return nil
	}
	type ver struct {
		major, minor int
		str          string
	}
	var found []ver
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if sm := dirVerRe.FindStringSubmatch(e.Name()); sm != nil {
			maj, _ := strconv.Atoi(sm[1])
			min, _ := strconv.Atoi(sm[2])
			found = append(found, ver{maj, min, sm[1] + "." + sm[2]})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].major != found[j].major {
			return found[i].major > found[j].major
		}
		return found[i].minor > found[j].minor
	})
	out := make([]string, len(found))
	for i, v := range found {
		out[i] = v.str
	}
	return out
}

func (m *Manager) NewestPHP() (string, error) {
	versions := m.InstalledVersions()
	if len(versions) == 0 {
		return "", fmt.Errorf("no PHP runtime in %s (install the suite with 'igor install')", m.cfg.PHPRoot)
	}
	return versions[0], nil
}

func (m *Manager) IsInstalled(version string) bool {
	info, err := os.Stat(filepath.Join(m.cfg.PHPBin(version), "frankenphp"))
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func (m *Manager) InstalledPHPPatch(version string) string {
	php := filepath.Join(m.cfg.PHPBin(version), "php")
	info, err := os.Stat(php)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return ""
	}
	res := m.run.Run(php, "-r", "echo PHP_VERSION;")
	if !res.OK() {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

func (m *Manager) ReadPHPVersionFile(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, ".php-version"))
	if err != nil {
		return ""
	}
	s := strings.Join(strings.Fields(string(raw)), "")
	s = strings.TrimPrefix(s, "php")
	if sm := phpVerStartRe.FindStringSubmatch(s); sm != nil {
		return sm[1]
	}
	return s
}

func (m *Manager) ComposerPlatformPHP(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "composer.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Config struct {
			Platform struct {
				PHP string `json:"php"`
			} `json:"platform"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	if sm := platformRe.FindStringSubmatch(doc.Config.Platform.PHP); sm != nil {
		return sm[1]
	}
	return ""
}

func (m *Manager) MarkerGet(version, key string) string {
	raw, err := os.ReadFile(filepath.Join(m.cfg.PHPRoot, "php"+version, ".igor-release"))
	if err != nil {
		return ""
	}
	for _, pair := range strings.Fields(string(raw)) {
		if strings.HasPrefix(pair, key+"=") {
			return strings.TrimPrefix(pair, key+"=")
		}
	}
	return ""
}

func (m *Manager) MarkerWrite(version, tag, frankenphp, phpPatch string) error {
	path := filepath.Join(m.cfg.PHPRoot, "php"+version, ".igor-release")
	content := fmt.Sprintf("tag=%s frankenphp=%s php=%s\n", tag, frankenphp, phpPatch)
	return os.WriteFile(path, []byte(content), 0o644)
}

func (m *Manager) BackupsFor(version string) []string {
	matches, _ := filepath.Glob(filepath.Join(m.cfg.PHPRoot, "php"+version+".backup.*"))
	sort.Slice(matches, func(i, j int) bool {
		si, _ := os.Stat(matches[i])
		sj, _ := os.Stat(matches[j])
		if si == nil || sj == nil {
			return matches[i] > matches[j]
		}
		return si.ModTime().After(sj.ModTime())
	})
	return matches
}
