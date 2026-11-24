package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/value-quest/igor/internal/runner"
)

func (a *App) cmdStart() *cobra.Command {
	return &cobra.Command{
		Use:   "start [<name>]",
		Short: "Start a site/service/worker, or everything when called without arguments",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				if err := a.Control("start", args[0]); err != nil {
					return err
				}
				a.UI.Info(args[0] + " started")
				a.checkDNS()
				return nil
			}

			if !a.Launchd.IsRunning("proxy") && (a.Run.PortInUse("80") || a.Run.PortInUse("443")) {
				return fmt.Errorf("ports 80/443 are in use - quit Herd (or whatever holds them) first")
			}

			a.UI.Info("Front proxy")
			if err := a.Launchd.Start("proxy"); err != nil {
				return err
			}
			names, err := a.Registry.RunnableNames()
			if err != nil {
				return err
			}
			for _, n := range names {
				if err := a.Control("start", n); err != nil {
					return err
				}
				a.UI.Note(n + " started")
			}
			a.checkDNS()
			a.UI.Outro(fmt.Sprintf("igor is running: https://<name>.%s", a.Cfg.TLD))
			return nil
		},
	}
}

func (a *App) checkDNS() {
	resolverFile := a.Cfg.ResolverPath()
	raw, err := os.ReadFile(resolverFile)
	if err != nil || strings.TrimSpace(string(raw)) != "nameserver 127.0.0.1" {
		a.UI.Warn(fmt.Sprintf("Resolver %s is missing or wrong (often removed by Herd) - *.%s domains won't resolve", resolverFile, a.Cfg.TLD))
		if a.UI.Interactive {
			ok, confirmErr := a.UI.Confirm(fmt.Sprintf("Recreate %s now (needs sudo)?", resolverFile), true)
			if confirmErr == nil && ok {
				mkRes := a.Run.RunWith(runner.Opts{Interactive: true}, "sudo", "mkdir", "-p", filepath.Dir(resolverFile))
				teeRes := a.Run.RunWith(runner.Opts{Stdin: strings.NewReader("nameserver 127.0.0.1\n")}, "sudo", "tee", resolverFile)
				if mkRes.OK() && teeRes.OK() {
					a.UI.Info("Recreated " + resolverFile)
				} else {
					a.UI.Warn(fmt.Sprintf("Could not recreate %s - run 'igor setup'", resolverFile))
				}
			} else {
				a.UI.Note(fmt.Sprintf("Fix with 'igor setup' or: echo \"nameserver 127.0.0.1\" | sudo tee %s", resolverFile))
			}
		} else {
			a.UI.Note(fmt.Sprintf("Fix with 'igor setup' or: echo \"nameserver 127.0.0.1\" | sudo tee %s", resolverFile))
		}
	}

	if !a.Run.Run("pgrep", "-x", "dnsmasq").OK() {
		a.UI.Warn(fmt.Sprintf("dnsmasq is not running - *.%s domains won't resolve", a.Cfg.TLD))
		if a.UI.Interactive {
			ok, confirmErr := a.UI.Confirm("Start dnsmasq now (needs sudo)?", true)
			if confirmErr == nil && ok {
				if a.Run.RunWith(runner.Opts{Interactive: true}, "sudo", "brew", "services", "restart", "dnsmasq").OK() {
					a.UI.Info("Started dnsmasq")
				} else {
					a.UI.Warn("Failed to start dnsmasq - run 'sudo brew services restart dnsmasq' or 'igor setup'")
				}
			} else {
				a.UI.Note("Fix with 'sudo brew services restart dnsmasq' or 'igor setup'")
			}
		} else {
			a.UI.Note("Fix with 'sudo brew services restart dnsmasq' or 'igor setup'")
		}
	}
}

func (a *App) cmdStop() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [<name>]",
		Short: "Stop a site/service/worker, or everything when called without arguments",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				if err := a.Control("stop", args[0]); err != nil {
					return err
				}
				a.UI.Info(args[0] + " stopped")
				return nil
			}

			names, err := a.Registry.RunnableNames()
			if err != nil {
				return err
			}
			for _, n := range names {
				if err := a.Control("stop", n); err != nil {
					return err
				}
				a.UI.Note(n + " stopped")
			}
			a.UI.Info("Front proxy")
			if err := a.Launchd.Disable("proxy"); err != nil {
				return err
			}
			a.UI.Outro("igor is stopped and stays stopped after a restart. Ports 80/443 are free, so you can start Herd now. Back to igor: quit Herd, then 'igor start'.")
			return nil
		},
	}
}

func (a *App) cmdRestart() *cobra.Command {
	return &cobra.Command{
		Use:   "restart [<name>]",
		Short: "Restart a site and its workers, a single worker, or a service",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if name == "" {
				names, _ := a.Registry.RunnableNames()
				if !a.UI.Interactive || len(names) == 0 {
					return fmt.Errorf("usage: igor restart <name>")
				}
				var opts []huh.Option[string]
				for _, n := range names {
					opts = append(opts, huh.NewOption(n, n))
					for _, w := range a.Workers.SiteWorkers(n) {
						d := a.Cfg.DisplayAgentName(w)
						opts = append(opts, huh.NewOption("  "+d, d))
					}
				}
				var err error
				name, err = a.UI.Select("Which site, worker, or service should be restarted?", opts)
				if err != nil {
					return fmt.Errorf("cancelled")
				}
			}
			if err := a.Control("restart", name); err != nil {
				return err
			}
			a.UI.Info(name + " restarted")
			return nil
		},
	}
}

func (a *App) cmdList() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all sites, workers, services and routes with state",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runList()
		},
	}
}

func (a *App) runList() error {
	entries, err := a.Registry.All()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		a.UI.Note("Nothing yet - run 'igor add <name>' in a project.")
		return nil
	}

	workersBySite := map[string][]string{}
	agents := []string{"proxy"}
	ports := map[string]bool{}

	for _, e := range entries {
		if e.Type == "proxy" {
			ports[e.Port] = true
		} else {
			agents = append(agents, e.Name)
			if e.Type == "php" {
				ws := a.Workers.SiteWorkers(e.Name)
				workersBySite[e.Name] = ws
				agents = append(agents, ws...)
			}
		}
	}

	// Query all launchd agents and proxy ports concurrently with goroutines
	var mu sync.Mutex
	var wg sync.WaitGroup
	agentState := make(map[string]bool, len(agents))
	portState := make(map[string]bool, len(ports))

	for _, ag := range agents {
		wg.Add(1)
		go func(agent string) {
			defer wg.Done()
			running := a.Launchd.IsRunning(agent)
			mu.Lock()
			agentState[agent] = running
			mu.Unlock()
		}(ag)
	}
	for p := range ports {
		wg.Add(1)
		go func(port string) {
			defer wg.Done()
			up := a.Run.PortInUse(port)
			mu.Lock()
			portState[port] = up
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	var rows [][]string
	for _, e := range entries {
		var phpCol, state, dirCol string
		switch e.Type {
		case "proxy":
			phpCol = e.PHP
			dirCol = e.Dir
			if portState[e.Port] {
				state = "up"
			} else {
				state = "down"
			}
		case "service":
			phpCol = "svc"
			dirCol = e.Dir
			if agentState[e.Name] {
				state = "running"
			} else {
				state = "stopped"
			}
		default:
			phpCol = e.PHP
			if agentState[e.Name] {
				state = "running"
			} else {
				state = "stopped"
			}
			if e.EffectiveDocroot() == "public" {
				dirCol = e.Dir
			} else {
				dirCol = fmt.Sprintf("%s  (docroot: %s)", e.Dir, e.EffectiveDocroot())
			}
		}

		rows = append(rows, []string{e.Name, e.Port, phpCol, state, dirCol})
		if e.Type == "php" {
			for _, w := range workersBySite[e.Name] {
				short := w[strings.Index(w, "@")+1:]
				wState := "stopped"
				if agentState[w] {
					wState = "running"
				}
				rows = append(rows, []string{"  :" + short, "", "", wState, ""})
			}
		}
	}

	a.UI.Table([]string{"NAME", "PORT", "PHP", "STATE", "DIR"}, rows)

	proxyState := "stopped"
	if agentState["proxy"] {
		proxyState = "running"
	}
	a.UI.Info("Front proxy: " + proxyState)
	return nil
}

func (a *App) cmdLogs() *cobra.Command {
	var clearFlag bool
	cmd := &cobra.Command{
		Use:   "logs [<name>]",
		Short: "Follow a log (<name>, <name>:<worker>, <name>:watch, or proxy) or clear logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}

			if clearFlag {
				if name == "" {
					files, _ := filepath.Glob(filepath.Join(a.Cfg.LogsDir, "*.log"))
					for _, f := range files {
						_ = os.WriteFile(f, nil, 0o644)
					}
					a.UI.Info("Cleared all logs in " + a.Cfg.LogsDir)
					return nil
				}
				f := a.Cfg.LogPath(a.Cfg.AgentName(name))
				if _, err := os.Stat(f); err != nil {
					return fmt.Errorf("no log for '%s'", name)
				}
				if err := os.WriteFile(f, nil, 0o644); err != nil {
					return err
				}
				a.UI.Info("Cleared " + f)
				return nil
			}

			if name == "" {
				if !a.UI.Interactive {
					return fmt.Errorf("usage: igor logs <name|proxy>  or  igor logs --clear [<name>]")
				}
				files, _ := filepath.Glob(filepath.Join(a.Cfg.LogsDir, "*.log"))
				if len(files) == 0 {
					return fmt.Errorf("no logs in %s yet", a.Cfg.LogsDir)
				}
				var names []string
				for _, f := range files {
					names = append(names, a.Cfg.DisplayAgentName(strings.TrimSuffix(filepath.Base(f), ".log")))
				}
				sort.Strings(names)
				var opts []huh.Option[string]
				for _, n := range names {
					opts = append(opts, huh.NewOption(n, n))
				}
				var err error
				name, err = a.UI.Select("Which log would you like to follow?", opts)
				if err != nil {
					return fmt.Errorf("cancelled")
				}
			}

			f := a.Cfg.LogPath(a.Cfg.AgentName(name))
			if _, err := os.Stat(f); err != nil {
				return fmt.Errorf("no log for '%s' yet", name)
			}

			if a.UI.Interactive && !a.Run.IsFake() {
				return syscall.Exec("/usr/bin/tail", []string{"tail", "-f", f}, os.Environ())
			}
			a.Run.RunWith(runner.Opts{Interactive: a.UI.Interactive}, "tail", "-f", f)
			return nil
		},
	}
	cmd.Flags().BoolVar(&clearFlag, "clear", false, "Empty one log, or all of them")
	return cmd
}

func (a *App) cmdInfo() *cobra.Command {
	return &cobra.Command{
		Use:     "info [<name>]",
		Aliases: []string{"status"},
		Short:   "Show URLs, PHP version, and .env hints for internal access",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if name == "" && a.UI.Interactive {
				all, _ := a.Registry.All()
				if len(all) > 0 {
					var opts []huh.Option[string]
					for _, e := range all {
						opts = append(opts, huh.NewOption(fmt.Sprintf("%s (%s)", e.Name, e.Type), e.Name))
					}
					var err error
					name, err = a.UI.Select("Which site or service?", opts)
					if err != nil {
						return fmt.Errorf("cancelled")
					}
				}
			}

			entry, err := a.Registry.Get(name)
			if err != nil {
				return err
			}
			if entry == nil {
				return fmt.Errorf("no site '%s'", name)
			}

			if entry.Type == "service" {
				res := a.Run.Run("plutil", "-extract", "ProgramArguments", "json", "-o", "-", a.Cfg.PlistPath(name))
				cmdStr := "(unknown)"
				if res.OK() {
					cmdStr = strings.TrimSpace(res.Stdout)
				}
				lines := []string{
					"Command:   " + cmdStr,
					"Data:      " + entry.Dir,
					"Log:       " + a.Cfg.LogPath(name),
				}
				if entry.Port != "-" {
					lines = append(lines, fmt.Sprintf("Browser:   https://%s.%s -> http://127.0.0.1:%s", name, a.Cfg.TLD, entry.Port))
				}
				a.UI.Callout("Service: "+name, lines, false)
				return nil
			}

			lines := []string{
				fmt.Sprintf("Browser:   https://%s.%s", name, a.Cfg.TLD),
				fmt.Sprintf("Internal:  http://127.0.0.1:%s     (server-to-server, no TLS)", entry.Port),
			}
			if entry.Type == "php" {
				phpLine := entry.PHP
				if patch := a.Runtimes.InstalledPHPPatch(entry.PHP); patch != "" {
					phpLine = fmt.Sprintf("%s (%s)", entry.PHP, patch)
				}
				lines = append(lines, "PHP:       "+phpLine)
			}
			lines = append(lines,
				"",
				fmt.Sprintf(".env hints for apps talking to '%s' (Laravel-style examples):", name),
				"  # SSO: browser redirects public, back-channel internal",
				fmt.Sprintf("  SSO_URL=https://%s.%s", name, a.Cfg.TLD),
				fmt.Sprintf("  SSO_INTERNAL_URL=http://127.0.0.1:%s", entry.Port),
				"  # Reverb: server publishes internally, browser connects via the proxy",
				"  REVERB_HOST=127.0.0.1",
				"  REVERB_PORT="+entry.Port,
				"  REVERB_SCHEME=http",
				fmt.Sprintf("  VITE_REVERB_HOST=%s.%s", name, a.Cfg.TLD),
				"  VITE_REVERB_PORT=443",
				"  VITE_REVERB_SCHEME=https",
			)
			a.UI.Callout("Site: "+name, lines, false)
			return nil
		},
	}
}

func (a *App) cmdReload() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Reload the front proxy config",
		RunE: func(cmd *cobra.Command, args []string) error {
			reloaded, err := a.Caddy.ReloadProxy()
			if err != nil {
				return err
			}
			if reloaded {
				a.UI.Info("Front proxy reloaded")
			} else {
				a.UI.Warn("Front proxy is not running - run 'igor setup'")
			}
			return nil
		},
	}
}

func (a *App) cmdShims() *cobra.Command {
	return &cobra.Command{
		Use:   "shims",
		Short: "(Re)create php/composer/frankenphp shims in ~/.igor/bin",
		RunE: func(cmd *cobra.Command, args []string) error {
			shims, err := a.Shims.CreateShims()
			if err != nil {
				return err
			}
			a.UI.Info(fmt.Sprintf("Shims in %s: %s", a.Cfg.ShimDir, strings.Join(shims, " ")))
			if !a.Shims.IsOnPath() {
				a.UI.Callout("Add shims to PATH", []string{
					"Put them first on PATH, at the end of ~/.zshrc (after Herd's lines):",
					`  export PATH="$HOME/.igor/bin:$PATH"`,
				}, false)
			}
			return nil
		},
	}
}

func (a *App) cmdRefresh() *cobra.Command {
	var restartFlag bool
	cmd := &cobra.Command{
		Use:   "refresh [<name>]",
		Short: "Regenerate all configs (PHP versions, workers, routes, proxy) and restart what changed",
		RunE: func(cmd *cobra.Command, args []string) error {
			only := ""
			if len(args) > 0 {
				only = args[0]
			}
			return a.runRefresh(only, restartFlag)
		},
	}
	cmd.Flags().BoolVar(&restartFlag, "restart", false, "Restart everything running (e.g. after a PHP update)")
	return cmd
}

func (a *App) runRefresh(only string, force bool) error {
	if _, err := os.Stat(a.Cfg.Caddyfile); err != nil {
		return fmt.Errorf("run 'igor setup' first")
	}

	if only != "" {
		if _, err := a.Registry.RequirePHPSite(only); err != nil {
			return err
		}
		if a.Cfg.IsWatch {
			fmt.Fprintf(a.UI.Out, "--- %s .php-version, composer.json or Procfile.dev changed\n", time.Now().Format("2006-01-02 15:04:05"))
		}
		if err := a.RefreshSite(only, force); err != nil {
			return err
		}
		return a.ReloadProxyOrWarn()
	}

	if _, err := a.Shims.CreateShims(); err != nil {
		return err
	}
	a.UI.Info("Shims updated")

	newest, err := a.Runtimes.NewestPHP()
	if err != nil {
		return err
	}
	a.UI.Info(fmt.Sprintf("Front proxy (PHP %s)", newest))
	proxyChanged, err := a.Caddy.WriteProxyPlist()
	if err != nil {
		return err
	}
	if bounced, err := a.Launchd.Bounce("proxy", proxyChanged, force); err != nil {
		return err
	} else if bounced {
		a.UI.Note("  restarted proxy")
	}

	entries, err := a.Registry.All()
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.Type {
		case "php":
			if err := a.RefreshSite(e.Name, force); err != nil {
				return err
			}
		case "service":
			a.UI.Info(e.Name + " (service)")
			if e.Port != "-" {
				_ = a.Caddy.WriteRoute(e.Name, e.Port)
			} else {
				a.Caddy.RemoveRoute(e.Name)
			}
			if bounced, err := a.Launchd.Bounce(e.Name, false, force); err != nil {
				return err
			} else if bounced {
				a.UI.Note("  restarted " + e.Name)
			}
		case "proxy":
			_ = a.Caddy.WriteRoute(e.Name, e.Port)
		}
	}

	if err := a.ReloadProxyOrWarn(); err != nil {
		return err
	}
	return a.runList()
}
