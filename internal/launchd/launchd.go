package launchd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/runner"
)

type EnvVar struct {
	Key   string
	Value string
}

type Manager struct {
	cfg *config.Config
	run runner.Runner
}

func New(cfg *config.Config, run runner.Runner) *Manager {
	return &Manager{cfg: cfg, run: run}
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// WritePlist writes a LaunchAgent plist byte-for-byte compatible with the bash version,
// leaving an identical file untouched to avoid macOS "Background Activity" banners.
func (m *Manager) WritePlist(name, workdir, path string, args []string, extraEnv []EnvVar, watchPaths []string) (bool, error) {
	if err := m.cfg.EnsureDir(m.cfg.AgentsDir); err != nil {
		return false, err
	}
	if err := m.cfg.EnsureDir(m.cfg.LogsDir); err != nil {
		return false, err
	}

	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n")
	b.WriteString("<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", xmlEscape(m.cfg.Label(name)))
	b.WriteString("  <key>ProgramArguments</key>\n")
	b.WriteString("  <array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("  </array>\n")
	fmt.Fprintf(&b, "  <key>WorkingDirectory</key><string>%s</string>\n", xmlEscape(workdir))
	b.WriteString("  <key>EnvironmentVariables</key>\n")
	b.WriteString("  <dict>\n")
	fmt.Fprintf(&b, "    <key>PATH</key><string>%s</string>\n", xmlEscape(path))
	fmt.Fprintf(&b, "    <key>PHP_INI_SCAN_DIR</key><string>%s</string>\n", xmlEscape(m.cfg.IniDir))
	fmt.Fprintf(&b, "    <key>HOME</key><string>%s</string>\n", xmlEscape(m.cfg.Home))
	for _, kv := range extraEnv {
		fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", xmlEscape(kv.Key), xmlEscape(kv.Value))
	}
	b.WriteString("  </dict>\n")

	if len(watchPaths) > 0 {
		b.WriteString("  <key>WatchPaths</key>\n")
		b.WriteString("  <array>\n")
		for _, wp := range watchPaths {
			fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(wp))
		}
		b.WriteString("  </array>\n")
		b.WriteString("  <key>ProcessType</key><string>Background</string>\n")
	} else {
		b.WriteString("  <key>RunAtLoad</key><true/>\n")
		b.WriteString("  <key>KeepAlive</key><true/>\n")
		b.WriteString("  <key>ProcessType</key><string>Interactive</string>\n")
	}

	logPath := xmlEscape(m.cfg.LogPath(name))
	fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n", logPath)
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", logPath)
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")

	content := b.String()
	plistFile := m.cfg.PlistPath(name)
	if existing, err := os.ReadFile(plistFile); err == nil && string(existing) == content {
		return false, nil
	}

	tmp := plistFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, plistFile); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) IsRunning(agent string) bool {
	return m.run.Run("launchctl", "print", m.cfg.Domain()+"/"+m.cfg.Label(agent)).OK()
}

func (m *Manager) IsAlive(agent string) bool {
	res := m.run.Run("launchctl", "print", m.cfg.Domain()+"/"+m.cfg.Label(agent))
	if !res.OK() {
		return false
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(line, "\tstate = running") {
			return true
		}
	}
	return false
}

func (m *Manager) Start(agent string) error {
	target := m.cfg.Domain() + "/" + m.cfg.Label(agent)
	m.run.Run("launchctl", "enable", target)
	if m.IsRunning(agent) {
		return nil
	}
	res := m.run.Run("launchctl", "bootstrap", m.cfg.Domain(), m.cfg.PlistPath(agent))
	if !res.OK() {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return fmt.Errorf("failed to start %s: %s", agent, msg)
	}
	return nil
}

func (m *Manager) Stop(agent string) error {
	if !m.IsRunning(agent) {
		return nil
	}
	target := m.cfg.Domain() + "/" + m.cfg.Label(agent)
	m.run.Run("launchctl", "bootout", target)
	for i := 0; i < 150; i++ {
		if !m.IsRunning(agent) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%s did not stop within 30s (launchctl print %s)", agent, target)
}

func (m *Manager) Disable(agent string) error {
	if err := m.Stop(agent); err != nil {
		return err
	}
	m.run.Run("launchctl", "disable", m.cfg.Domain()+"/"+m.cfg.Label(agent))
	return nil
}

func (m *Manager) Remove(agent string) error {
	if err := m.Stop(agent); err != nil {
		return err
	}
	_ = os.Remove(m.cfg.PlistPath(agent))
	return nil
}

func (m *Manager) Bounce(agent string, changed, force bool) (bool, error) {
	if !m.IsRunning(agent) {
		return false, nil
	}
	if !changed && !force {
		return false, nil
	}
	if err := m.Stop(agent); err != nil {
		return false, err
	}
	if err := m.Start(agent); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) CheckAlive(name string, logOffset int64, wait time.Duration) error {
	if wait > 0 {
		time.Sleep(wait)
	}
	if m.IsAlive(name) {
		return nil
	}

	logFile := m.cfg.LogPath(name)
	excerpt := "  |   (empty)"
	if raw, err := os.ReadFile(logFile); err == nil {
		src := string(raw)
		if logOffset > 0 && int64(len(raw)) > logOffset {
			delta := string(raw[logOffset:])
			if strings.TrimSpace(delta) != "" {
				src = delta
			}
		}
		lines := strings.Split(strings.TrimRight(src, "\r\n"), "\n")
		if len(lines) > 15 {
			lines = lines[len(lines)-15:]
		}
		if strings.TrimSpace(src) != "" {
			for i, l := range lines {
				lines[i] = "  | " + l
			}
			excerpt = strings.Join(lines, "\n")
		}
	}

	return fmt.Errorf(
		"%s is not running - it exited right after starting. Its log:\n%s\nlaunchd keeps retrying every 10s. Fix the command and run 'igor service add %s ...' again,\nor remove it with 'igor rm %s'.",
		name, excerpt, name, name,
	)
}
