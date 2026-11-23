package platform

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/registry"
	"github.com/value-quest/igor/internal/runner"
	"github.com/value-quest/igor/internal/ui"
)

type Kind string

const (
	Darwin Kind = "darwin"
	Linux  Kind = "linux"
	WSL2   Kind = "wsl2"
)

func Detect() Kind {
	if goruntime.GOOS == "darwin" {
		return Darwin
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return WSL2
	}
	if raw, err := os.ReadFile("/proc/version"); err == nil {
		lower := strings.ToLower(string(raw))
		if strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl") {
			return WSL2
		}
	}
	return Linux
}

type Platform struct {
	Kind Kind
	cfg  *config.Config
	run  runner.Runner
	ui   *ui.UI
	reg  *registry.Store
}

func New(kind Kind, cfg *config.Config, run runner.Runner, u *ui.UI, reg *registry.Store) *Platform {
	return &Platform{Kind: kind, cfg: cfg, run: run, ui: u, reg: reg}
}

func (p *Platform) IsDarwin() bool { return p.Kind == Darwin }
func (p *Platform) IsLinux() bool  { return p.Kind == Linux || p.Kind == WSL2 }
func (p *Platform) IsWSL() bool    { return p.Kind == WSL2 }

func (p *Platform) ServicePATH() string {
	if p.IsDarwin() {
		return "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
}

func (p *Platform) ExtraLookPaths() []string {
	if p.IsDarwin() {
		return []string{"/opt/homebrew/bin", "/usr/local/bin"}
	}
	return []string{"/usr/local/bin", "/usr/bin"}
}

func (p *Platform) InstallHint(exe string) string {
	if p.IsDarwin() {
		return fmt.Sprintf("brew install %s", exe)
	}
	return fmt.Sprintf("sudo apt install %s", exe)
}

func (p *Platform) PortInUseMsg() string {
	if p.IsDarwin() {
		return "ports 80/443 are in use - quit Herd (or whatever holds them) first"
	}
	return "ports 80/443 are in use - stop whatever holds them first (e.g. nginx, apache2)"
}

func (p *Platform) StopOutro() string {
	if p.IsDarwin() {
		return "igor is stopped and stays stopped after a restart. Ports 80/443 are free, so you can start Herd now. Back to igor: quit Herd, then 'igor start'."
	}
	return "igor is stopped and stays stopped after a restart. Ports 80/443 are free. Start again with 'igor start'."
}

func (p *Platform) Notify(msg string) {
	if !p.cfg.IsWatch {
		return
	}
	switch p.Kind {
	case Darwin:
		escaped := strings.ReplaceAll(msg, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		p.run.Run("osascript", "-e", fmt.Sprintf(`display notification "%s" with title "igor"`, escaped))
	case Linux:
		if _, err := p.run.LookPath("notify-send"); err == nil {
			p.run.Run("notify-send", "igor", msg)
		}
	}
}

func (p *Platform) EnsureProxyCapabilities(proxyBin string) error {
	if !p.IsLinux() || proxyBin == "" {
		return nil
	}
	if res := p.run.Run("getcap", proxyBin); res.OK() && strings.Contains(res.Stdout, "cap_net_bind_service") {
		return nil
	}
	p.ui.Info(fmt.Sprintf("Granting ports 80/443 capability to %s (sudo setcap)", proxyBin))
	res := p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, "sudo", "setcap", "cap_net_bind_service=+ep", proxyBin)
	if !res.OK() {
		p.ui.Warn(fmt.Sprintf("setcap failed - run manually: sudo setcap cap_net_bind_service=+ep %s", proxyBin))
	}
	return nil
}

func (p *Platform) TrustCA(proxyBin string) {
	if p.IsDarwin() {
		p.ui.Info("Trusting the local CA (macOS asks for your password)")
	} else {
		p.ui.Info("Trusting the local CA")
	}
	if !p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, proxyBin, "trust").OK() {
		p.ui.Warn(fmt.Sprintf("Trust failed - retry with: %s trust", proxyBin))
	}

	if p.IsWSL() {
		p.trustCAWindows()
	}
}

func (p *Platform) trustCAWindows() {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(p.cfg.Home, ".local", "share")
	}
	certPath := filepath.Join(dataHome, "caddy", "pki", "authorities", "local", "root.crt")
	if _, err := os.Stat(certPath); err != nil {
		p.ui.Note("Windows CA trust: Caddy root.crt not found yet (start the proxy, then run 'igor setup')")
		return
	}
	wslRes := p.run.Run("wslpath", "-w", certPath)
	if !wslRes.OK() {
		return
	}
	winCert := strings.TrimSpace(wslRes.Stdout)
	p.ui.Info("Trusting the local CA in Windows user certificate store (for Chrome/Edge)")
	if !p.run.Run("certutil.exe", "-addstore", "-user", "Root", winCert).OK() {
		p.ui.Warn(fmt.Sprintf("Could not trust CA in Windows automatically - run in PowerShell: certutil -addstore -user Root \"%s\"", winCert))
	}
}

const (
	hostsStartMarker = "# igor-start"
	hostsEndMarker   = "# igor-end"
)

// SyncHosts updates the managed igor block in the Windows hosts file on WSL2.
// No-op on macOS and native Linux (where wildcard dnsmasq handles *.<tld>).
func (p *Platform) SyncHosts() error {
	if !p.IsWSL() {
		return nil
	}
	entries, err := p.reg.All()
	if err != nil {
		return err
	}
	var domains []string
	for _, e := range entries {
		if e.Type == "service" && e.Port == "-" {
			continue
		}
		domains = append(domains, fmt.Sprintf("%s.%s", e.Name, p.cfg.TLD))
	}
	sort.Strings(domains)

	hostsPath := p.cfg.WindowsHostsPath
	if hostsPath == "" {
		hostsPath = "/mnt/c/Windows/System32/drivers/etc/hosts"
	}

	raw, err := os.ReadFile(hostsPath)
	if err != nil {
		p.ui.Warn(fmt.Sprintf("Could not read Windows hosts file (%s): %v", hostsPath, err))
		return nil
	}

	updated := replaceHostsBlock(string(raw), domains)
	if updated == string(raw) {
		return nil
	}

	if err := os.WriteFile(hostsPath, []byte(updated), 0o644); err != nil {
		p.ui.Warn(fmt.Sprintf("Could not write %s (Windows requires permissions) - grant once in an elevated PowerShell:", hostsPath))
		p.ui.Note(`  icacls C:\Windows\System32\drivers\etc\hosts /grant "$($env:USERNAME):(M)"`)
		if len(domains) > 0 {
			p.ui.Note("Or add manually: 127.0.0.1 " + strings.Join(domains, " "))
		}
		return nil
	}
	return nil
}

func replaceHostsBlock(content string, domains []string) string {
	var block strings.Builder
	if len(domains) > 0 {
		block.WriteString(hostsStartMarker + "\n")
		for _, d := range domains {
			fmt.Fprintf(&block, "127.0.0.1 %s\n", d)
		}
		block.WriteString(hostsEndMarker + "\n")
	}

	startIdx := strings.Index(content, hostsStartMarker)
	endIdx := strings.Index(content, hostsEndMarker)
	if startIdx >= 0 && endIdx > startIdx {
		afterEnd := endIdx + len(hostsEndMarker)
		if afterEnd < len(content) && content[afterEnd] == '\r' {
			afterEnd++
		}
		if afterEnd < len(content) && content[afterEnd] == '\n' {
			afterEnd++
		}
		return content[:startIdx] + block.String() + content[afterEnd:]
	}

	if block.Len() == 0 {
		return content
	}
	sep := ""
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		sep = "\n"
	}
	return content + sep + "\n" + block.String()
}

func (p *Platform) SetupEnvironment() error {
	if p.IsDarwin() {
		return p.setupDarwin()
	}
	return p.setupLinux()
}

func (p *Platform) setupDarwin() error {
	if _, err := p.run.LookPath("brew"); err != nil {
		return fmt.Errorf("Homebrew is required")
	}

	if _, err := p.run.LookPath("direnv"); err != nil {
		if err := p.ui.Spin("Installing direnv via Homebrew...", func() error {
			if res := p.run.Run("brew", "install", "direnv"); !res.OK() {
				return fmt.Errorf("brew install direnv failed: %s", res.Stderr)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	if !p.run.Run("brew", "list", "dnsmasq").OK() {
		if err := p.ui.Spin("Installing dnsmasq via Homebrew...", func() error {
			if res := p.run.Run("brew", "install", "dnsmasq"); !res.OK() {
				return fmt.Errorf("brew install dnsmasq failed: %s", res.Stderr)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	prefix := strings.TrimSpace(p.run.Run("brew", "--prefix").Stdout)
	conf := filepath.Join(prefix, "etc", "dnsmasq.conf")
	existing, _ := os.ReadFile(conf)
	if !strings.Contains(string(existing), "# igor") {
		p.ui.Info(fmt.Sprintf("Pointing *.%s to 127.0.0.1 in %s", p.cfg.TLD, conf))
		f, err := os.OpenFile(conf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(f, "\n# igor\naddress=/.%s/127.0.0.1\nlisten-address=127.0.0.1\n", p.cfg.TLD)
		f.Close()
	}

	p.ui.Info("Starting dnsmasq (needs sudo for port 53)")
	if res := p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, "sudo", "brew", "services", "restart", "dnsmasq"); !res.OK() {
		return fmt.Errorf("failed to restart dnsmasq")
	}

	resolverFile := p.cfg.ResolverPath()
	curRes, _ := os.ReadFile(resolverFile)
	if strings.TrimSpace(string(curRes)) != "nameserver 127.0.0.1" {
		p.ui.Info(fmt.Sprintf("Creating %s (sudo)", resolverFile))
		p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, "sudo", "mkdir", "-p", filepath.Dir(resolverFile))
		p.run.RunWith(runner.Opts{Stdin: strings.NewReader("nameserver 127.0.0.1\n")}, "sudo", "tee", resolverFile)
	}

	if p.run.Run("pgrep", "-qf", "Herd").OK() {
		p.ui.Warn("Herd seems to be running - quit it, it holds ports 80/443.")
	}
	return nil
}

func (p *Platform) setupLinux() error {
	if !p.run.Run("systemctl", "--user", "status").OK() && !p.run.Run("systemctl", "--user", "list-units", "--no-pager").OK() {
		if p.IsWSL() {
			return fmt.Errorf("systemd --user is not active in WSL2. Enable it in /etc/wsl.conf:\n  [boot]\n  systemd=true\nThen run 'wsl --shutdown' from PowerShell and reopen your WSL terminal.")
		}
		return fmt.Errorf("systemd --user is not available - igor on Linux requires a systemd user session")
	}

	if _, err := p.run.LookPath("direnv"); err != nil {
		p.ui.Warn("direnv is not installed (recommended: sudo apt install direnv)")
	}

	if !p.IsWSL() {
		// Native Linux: configure dnsmasq + systemd-resolved drop-in if present
		if _, err := p.run.LookPath("dnsmasq"); err == nil {
			dnsmasqDropin := "/etc/dnsmasq.d/igor.conf"
			want := fmt.Sprintf("# igor\naddress=/.%s/127.0.0.1\nlisten-address=127.0.0.1\nbind-interfaces\n", p.cfg.TLD)
			cur, _ := os.ReadFile(dnsmasqDropin)
			if string(cur) != want {
				p.ui.Info(fmt.Sprintf("Configuring *.%s -> 127.0.0.1 in %s (sudo)", p.cfg.TLD, dnsmasqDropin))
				p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, "sudo", "mkdir", "-p", "/etc/dnsmasq.d")
				p.run.RunWith(runner.Opts{Stdin: strings.NewReader(want)}, "sudo", "tee", dnsmasqDropin)
				p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, "sudo", "systemctl", "enable", "--now", "dnsmasq")
				p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, "sudo", "systemctl", "restart", "dnsmasq")
			}

			if p.run.Run("systemctl", "is-active", "--quiet", "systemd-resolved").OK() {
				resolvedConf := "/etc/systemd/resolved.conf.d/igor.conf"
				wantRes := fmt.Sprintf("[Resolve]\nDNS=127.0.0.1\nDomains=~%s\n", p.cfg.TLD)
				curRes, _ := os.ReadFile(resolvedConf)
				if string(curRes) != wantRes {
					p.ui.Info(fmt.Sprintf("Configuring systemd-resolved for *.%s in %s (sudo)", p.cfg.TLD, resolvedConf))
					p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, "sudo", "mkdir", "-p", "/etc/systemd/resolved.conf.d")
					p.run.RunWith(runner.Opts{Stdin: strings.NewReader(wantRes)}, "sudo", "tee", resolvedConf)
					p.run.RunWith(runner.Opts{Interactive: p.ui.Interactive}, "sudo", "systemctl", "restart", "systemd-resolved")
				}
			}
		} else {
			p.ui.Warn(fmt.Sprintf("dnsmasq is not installed - install it (e.g. sudo apt install dnsmasq) and re-run 'igor setup' for *.%s DNS", p.cfg.TLD))
		}

		// Best-effort enable user lingering so services survive logouts / start on boot
		if user := os.Getenv("USER"); user != "" {
			p.run.Run("loginctl", "enable-linger", user)
		}
	} else {
		p.ui.Info("WSL2 detected: Windows browsers resolve sites via Windows hosts (synced on 'igor add'/'rm')")
		_ = p.SyncHosts()
	}

	return nil
}

func (p *Platform) CheckDNS() {
	if p.IsDarwin() {
		p.checkDNSDarwin()
		return
	}
	if p.IsWSL() {
		hostsPath := p.cfg.WindowsHostsPath
		if hostsPath == "" {
			hostsPath = "/mnt/c/Windows/System32/drivers/etc/hosts"
		}
		if entries, err := p.reg.All(); err == nil && len(entries) > 0 {
			if raw, err := os.ReadFile(hostsPath); err != nil || !strings.Contains(string(raw), hostsStartMarker) {
				p.ui.Warn(fmt.Sprintf("Windows hosts file (%s) is missing igor entries - *.%s may not resolve in Windows browsers", hostsPath, p.cfg.TLD))
				p.ui.Note("Run 'igor setup' or grant write permissions to hosts in an elevated PowerShell")
			}
		}
		return
	}

	// Native Linux
	if !p.run.Run("pgrep", "-x", "dnsmasq").OK() {
		p.ui.Warn(fmt.Sprintf("dnsmasq is not running - *.%s domains won't resolve", p.cfg.TLD))
		p.ui.Note("Fix with 'sudo systemctl restart dnsmasq' or 'igor setup'")
	}
}

func (p *Platform) checkDNSDarwin() {
	resolverFile := p.cfg.ResolverPath()
	raw, err := os.ReadFile(resolverFile)
	if err != nil || strings.TrimSpace(string(raw)) != "nameserver 127.0.0.1" {
		p.ui.Warn(fmt.Sprintf("Resolver %s is missing or wrong (often removed by Herd) - *.%s domains won't resolve", resolverFile, p.cfg.TLD))
		if p.ui.Interactive {
			ok, confirmErr := p.ui.Confirm(fmt.Sprintf("Recreate %s now (needs sudo)?", resolverFile), true)
			if confirmErr == nil && ok {
				mkRes := p.run.RunWith(runner.Opts{Interactive: true}, "sudo", "mkdir", "-p", filepath.Dir(resolverFile))
				teeRes := p.run.RunWith(runner.Opts{Stdin: strings.NewReader("nameserver 127.0.0.1\n")}, "sudo", "tee", resolverFile)
				if mkRes.OK() && teeRes.OK() {
					p.ui.Info("Recreated " + resolverFile)
				} else {
					p.ui.Warn(fmt.Sprintf("Could not recreate %s - run 'igor setup'", resolverFile))
				}
			} else {
				p.ui.Note(fmt.Sprintf("Fix with 'igor setup' or: echo \"nameserver 127.0.0.1\" | sudo tee %s", resolverFile))
			}
		} else {
			p.ui.Note(fmt.Sprintf("Fix with 'igor setup' or: echo \"nameserver 127.0.0.1\" | sudo tee %s", resolverFile))
		}
	}

	if !p.run.Run("pgrep", "-x", "dnsmasq").OK() {
		p.ui.Warn(fmt.Sprintf("dnsmasq is not running - *.%s domains won't resolve", p.cfg.TLD))
		if p.ui.Interactive {
			ok, confirmErr := p.ui.Confirm("Start dnsmasq now (needs sudo)?", true)
			if confirmErr == nil && ok {
				if p.run.RunWith(runner.Opts{Interactive: true}, "sudo", "brew", "services", "restart", "dnsmasq").OK() {
					p.ui.Info("Started dnsmasq")
				} else {
					p.ui.Warn("Failed to start dnsmasq - run 'sudo brew services restart dnsmasq' or 'igor setup'")
				}
			} else {
				p.ui.Note("Fix with 'sudo brew services restart dnsmasq' or 'igor setup'")
			}
		} else {
			p.ui.Note("Fix with 'sudo brew services restart dnsmasq' or 'igor setup'")
		}
	}
}

func (p *Platform) DoctorEnvChecks(proxyBin string) (rows [][]string, failures, warnings int) {
	if p.IsDarwin() {
		return p.doctorDarwin()
	}
	return p.doctorLinux(proxyBin)
}

func (p *Platform) doctorDarwin() (rows [][]string, failures, warnings int) {
	var (
		brewPath       string
		direnvPath     string
		brewPrefix     string
		hasDnsmasq     bool
		dnsmasqRunning bool
		herdRunning    bool
		wg             sync.WaitGroup
	)

	wg.Add(6)
	go func() { defer wg.Done(); brewPath, _ = p.run.LookPath("brew") }()
	go func() { defer wg.Done(); direnvPath, _ = p.run.LookPath("direnv") }()
	go func() { defer wg.Done(); brewPrefix = strings.TrimSpace(p.run.Run("brew", "--prefix").Stdout) }()
	go func() { defer wg.Done(); hasDnsmasq = p.run.Run("brew", "list", "dnsmasq").OK() }()
	go func() { defer wg.Done(); dnsmasqRunning = p.run.Run("pgrep", "-x", "dnsmasq").OK() }()
	go func() { defer wg.Done(); herdRunning = p.run.Run("pgrep", "-qf", "Herd").OK() }()
	wg.Wait()

	if brewPath != "" {
		rows = append(rows, []string{"Homebrew", "OK", brewPath})
	} else {
		rows = append(rows, []string{"Homebrew", "FAIL", "Not found (install from https://brew.sh)"})
		failures++
	}

	if direnvPath != "" {
		rows = append(rows, []string{"direnv", "OK", direnvPath})
	} else {
		rows = append(rows, []string{"direnv", "WARN", "Not found (run 'igor setup' or 'brew install direnv')"})
		warnings++
	}

	dnsmasqConf := ""
	hasRule := false
	if brewPrefix != "" {
		dnsmasqConf = filepath.Join(brewPrefix, "etc", "dnsmasq.conf")
		if raw, err := os.ReadFile(dnsmasqConf); err == nil {
			hasRule = strings.Contains(string(raw), fmt.Sprintf("address=/.%s/127.0.0.1", p.cfg.TLD))
		}
	}
	switch {
	case hasDnsmasq && hasRule && dnsmasqRunning:
		rows = append(rows, []string{"dnsmasq", "OK", fmt.Sprintf("*.%s -> 127.0.0.1 (%s)", p.cfg.TLD, dnsmasqConf)})
	case hasDnsmasq && hasRule:
		rows = append(rows, []string{"dnsmasq", "FAIL", "Configured, but not running (run 'sudo brew services restart dnsmasq' or 'igor setup')"})
		failures++
	case hasDnsmasq:
		rows = append(rows, []string{"dnsmasq", "WARN", fmt.Sprintf("Installed, but *.%s rule missing (run 'igor setup')", p.cfg.TLD)})
		warnings++
	default:
		rows = append(rows, []string{"dnsmasq", "FAIL", "Not installed (run 'igor setup')"})
		failures++
	}

	resolverFile := p.cfg.ResolverPath()
	if raw, err := os.ReadFile(resolverFile); err == nil && strings.TrimSpace(string(raw)) == "nameserver 127.0.0.1" {
		rows = append(rows, []string{fmt.Sprintf("Resolver (%s)", resolverFile), "OK", "nameserver 127.0.0.1"})
	} else {
		rows = append(rows, []string{fmt.Sprintf("Resolver (%s)", resolverFile), "FAIL", "Missing or wrong (run 'igor setup')"})
		failures++
	}

	if herdRunning {
		rows = append(rows, []string{"Herd conflict", "WARN", "Herd appears to be running and may hold ports 80/443"})
		warnings++
	} else {
		rows = append(rows, []string{"Herd conflict", "OK", "No Herd process detected"})
	}

	return rows, failures, warnings
}

func (p *Platform) doctorLinux(proxyBin string) (rows [][]string, failures, warnings int) {
	if p.IsWSL() {
		rows = append(rows, []string{"Platform", "OK", "WSL2 (Linux)"})
	} else {
		rows = append(rows, []string{"Platform", "OK", "Linux"})
	}

	if p.run.Run("systemctl", "--user", "list-units", "--no-pager").OK() {
		rows = append(rows, []string{"systemd --user", "OK", "Active"})
	} else {
		hint := "Not active (systemd user session required)"
		if p.IsWSL() {
			hint = "Not active (set [boot] systemd=true in /etc/wsl.conf and run wsl --shutdown)"
		}
		rows = append(rows, []string{"systemd --user", "FAIL", hint})
		failures++
	}

	if direnvPath, err := p.run.LookPath("direnv"); err == nil && direnvPath != "" {
		rows = append(rows, []string{"direnv", "OK", direnvPath})
	} else {
		rows = append(rows, []string{"direnv", "WARN", "Not found (install via package manager, e.g. sudo apt install direnv)"})
		warnings++
	}

	if !p.IsWSL() {
		dnsmasqPath, _ := p.run.LookPath("dnsmasq")
		dnsmasqRunning := p.run.Run("pgrep", "-x", "dnsmasq").OK()
		switch {
		case dnsmasqPath != "" && dnsmasqRunning:
			rows = append(rows, []string{"dnsmasq", "OK", fmt.Sprintf("Running (%s)", dnsmasqPath)})
		case dnsmasqPath != "":
			rows = append(rows, []string{"dnsmasq", "FAIL", "Installed, but not running (run 'sudo systemctl restart dnsmasq' or 'igor setup')"})
			failures++
		default:
			rows = append(rows, []string{"dnsmasq", "WARN", "Not installed (install dnsmasq and run 'igor setup')"})
			warnings++
		}
	} else {
		hostsPath := p.cfg.WindowsHostsPath
		if hostsPath == "" {
			hostsPath = "/mnt/c/Windows/System32/drivers/etc/hosts"
		}
		if _, err := os.Stat(hostsPath); err == nil {
			rows = append(rows, []string{"Windows hosts", "OK", hostsPath})
		} else {
			rows = append(rows, []string{"Windows hosts", "WARN", fmt.Sprintf("%s not accessible", hostsPath)})
			warnings++
		}
	}

	if proxyBin != "" {
		if res := p.run.Run("getcap", proxyBin); res.OK() && strings.Contains(res.Stdout, "cap_net_bind_service") {
			rows = append(rows, []string{"Ports 80/443 (setcap)", "OK", proxyBin})
		} else {
			rows = append(rows, []string{"Ports 80/443 (setcap)", "WARN", fmt.Sprintf("Missing cap_net_bind_service (run 'igor setup' or sudo setcap cap_net_bind_service=+ep %s)", proxyBin)})
			warnings++
		}
	}

	return rows, failures, warnings
}
