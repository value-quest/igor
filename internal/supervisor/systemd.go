package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/runner"
)

type SystemdManager struct {
	cfg *config.Config
	run runner.Runner
}

func NewSystemd(cfg *config.Config, run runner.Runner) *SystemdManager {
	return &SystemdManager{cfg: cfg, run: run}
}

func (m *SystemdManager) unitDir() string {
	if m.cfg.SystemdDir != "" {
		return m.cfg.SystemdDir
	}
	return filepath.Join(m.cfg.Home, ".config", "systemd", "user")
}

// unitBase maps igor agent names to safe systemd unit basenames (avoiding '@' template syntax).
func (m *SystemdManager) unitBase(agent string) string {
	if strings.HasSuffix(agent, "~watch") {
		return config.LabelPrefix + "." + strings.TrimSuffix(agent, "~watch") + "--watch"
	}
	if idx := strings.IndexByte(agent, '@'); idx >= 0 {
		return config.LabelPrefix + "." + agent[:idx] + "--worker--" + agent[idx+1:]
	}
	return config.LabelPrefix + "." + agent
}

func (m *SystemdManager) isWatcher(agent string) bool {
	return strings.HasSuffix(agent, "~watch")
}

func (m *SystemdManager) primaryUnit(agent string) string {
	if m.isWatcher(agent) {
		return m.unitBase(agent) + ".path"
	}
	return m.unitBase(agent) + ".service"
}

func (m *SystemdManager) servicePath(agent string) string {
	return filepath.Join(m.unitDir(), m.unitBase(agent)+".service")
}

func (m *SystemdManager) pathUnitPath(agent string) string {
	return filepath.Join(m.unitDir(), m.unitBase(agent)+".path")
}

func (m *SystemdManager) UnitPath(agent string) string {
	return filepath.Join(m.unitDir(), m.primaryUnit(agent))
}

func (m *SystemdManager) Exists(agent string) bool {
	_, err := os.Stat(m.UnitPath(agent))
	return err == nil
}

func systemdQuoteArg(s string) string {
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, `%`, `%%`)
	if strings.ContainsAny(s, " \t\n\"'\\$%") || s == "" {
		return `"` + escaped + `"`
	}
	return escaped
}

func systemdQuoteEnv(k, v string) string {
	escaped := strings.ReplaceAll(v, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, `%`, `%%`)
	return fmt.Sprintf(`"%s=%s"`, k, escaped)
}

func writeIfChanged(file, content string) (bool, error) {
	if existing, err := os.ReadFile(file); err == nil && string(existing) == content {
		return false, nil
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, file); err != nil {
		return false, err
	}
	return true, nil
}

func (m *SystemdManager) WriteUnit(name, workdir, path string, args []string, extraEnv []EnvVar, watchPaths []string) (bool, error) {
	if err := m.cfg.EnsureDir(m.unitDir()); err != nil {
		return false, err
	}
	if err := m.cfg.EnsureDir(m.cfg.LogsDir); err != nil {
		return false, err
	}

	quotedArgs := make([]string, len(args))
	for i, a := range args {
		quotedArgs[i] = systemdQuoteArg(a)
	}
	execStart := strings.Join(quotedArgs, " ")
	logPath := m.cfg.LogPath(name)

	var svc strings.Builder
	svc.WriteString("[Unit]\n")
	fmt.Fprintf(&svc, "Description=igor managed service (%s)\n", name)
	svc.WriteString("After=network.target\n\n")
	svc.WriteString("[Service]\n")
	if len(watchPaths) > 0 {
		svc.WriteString("Type=oneshot\n")
	} else {
		svc.WriteString("Type=simple\n")
		svc.WriteString("Restart=always\n")
		svc.WriteString("RestartSec=10\n")
	}
	fmt.Fprintf(&svc, "WorkingDirectory=%s\n", workdir)
	fmt.Fprintf(&svc, "Environment=%s\n", systemdQuoteEnv("PATH", path))
	fmt.Fprintf(&svc, "Environment=%s\n", systemdQuoteEnv("PHP_INI_SCAN_DIR", m.cfg.IniDir))
	fmt.Fprintf(&svc, "Environment=%s\n", systemdQuoteEnv("HOME", m.cfg.Home))
	for _, kv := range extraEnv {
		fmt.Fprintf(&svc, "Environment=%s\n", systemdQuoteEnv(kv.Key, kv.Value))
	}
	fmt.Fprintf(&svc, "ExecStart=%s\n", execStart)
	fmt.Fprintf(&svc, "StandardOutput=append:%s\n", logPath)
	fmt.Fprintf(&svc, "StandardError=append:%s\n", logPath)
	if len(watchPaths) == 0 {
		svc.WriteString("\n[Install]\n")
		svc.WriteString("WantedBy=default.target\n")
	}

	svcChanged, err := writeIfChanged(m.servicePath(name), svc.String())
	if err != nil {
		return false, err
	}

	pathChanged := false
	if len(watchPaths) > 0 {
		var p strings.Builder
		p.WriteString("[Unit]\n")
		fmt.Fprintf(&p, "Description=igor file watcher (%s)\n\n", name)
		p.WriteString("[Path]\n")
		for _, wp := range watchPaths {
			fmt.Fprintf(&p, "PathChanged=%s\n", wp)
		}
		fmt.Fprintf(&p, "Unit=%s.service\n\n", m.unitBase(name))
		p.WriteString("[Install]\n")
		p.WriteString("WantedBy=default.target\n")

		pathChanged, err = writeIfChanged(m.pathUnitPath(name), p.String())
		if err != nil {
			return false, err
		}
	}

	if svcChanged || pathChanged {
		m.run.Run("systemctl", "--user", "daemon-reload")
	}
	return svcChanged || pathChanged, nil
}

func (m *SystemdManager) SiteWorkers(site string) []string {
	prefix := config.LabelPrefix + "." + site + "--worker--"
	pattern := filepath.Join(m.unitDir(), prefix+"*.service")
	files, _ := filepath.Glob(pattern)
	sort.Strings(files)

	var out []string
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".service")
		if strings.HasPrefix(base, prefix) {
			workerName := strings.TrimPrefix(base, prefix)
			out = append(out, site+"@"+workerName)
		}
	}
	return out
}

func (m *SystemdManager) CommandString(agent string) string {
	raw, err := os.ReadFile(m.servicePath(agent))
	if err != nil {
		return "(unknown)"
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
		}
	}
	return "(unknown)"
}

func (m *SystemdManager) IsRunning(agent string) bool {
	return m.run.Run("systemctl", "--user", "is-active", "--quiet", m.primaryUnit(agent)).OK()
}

func (m *SystemdManager) IsAlive(agent string) bool {
	return m.IsRunning(agent)
}

func (m *SystemdManager) Start(agent string) error {
	m.run.Run("systemctl", "--user", "daemon-reload")
	res := m.run.Run("systemctl", "--user", "enable", "--now", m.primaryUnit(agent))
	if !res.OK() {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return fmt.Errorf("failed to start %s: %s", agent, msg)
	}
	return nil
}

func (m *SystemdManager) Stop(agent string) error {
	if !m.IsRunning(agent) {
		return nil
	}
	res := m.run.Run("systemctl", "--user", "stop", m.primaryUnit(agent))
	if !res.OK() {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return fmt.Errorf("failed to stop %s: %s", agent, msg)
	}
	return nil
}

func (m *SystemdManager) Disable(agent string) error {
	m.run.Run("systemctl", "--user", "disable", "--now", m.primaryUnit(agent))
	return nil
}

func (m *SystemdManager) Remove(agent string) error {
	m.run.Run("systemctl", "--user", "disable", "--now", m.primaryUnit(agent))
	_ = os.Remove(m.servicePath(agent))
	if m.isWatcher(agent) {
		_ = os.Remove(m.pathUnitPath(agent))
	}
	m.run.Run("systemctl", "--user", "daemon-reload")
	return nil
}

func (m *SystemdManager) Bounce(agent string, changed, force bool) (bool, error) {
	if !m.IsRunning(agent) {
		return false, nil
	}
	if !changed && !force {
		return false, nil
	}
	m.run.Run("systemctl", "--user", "daemon-reload")
	res := m.run.Run("systemctl", "--user", "restart", m.primaryUnit(agent))
	if !res.OK() {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return false, fmt.Errorf("failed to restart %s: %s", agent, msg)
	}
	return true, nil
}

func (m *SystemdManager) CheckAlive(name string, logOffset int64, wait time.Duration) error {
	return checkAliveWith(m.cfg, "systemd", m.IsAlive, name, logOffset, wait)
}
