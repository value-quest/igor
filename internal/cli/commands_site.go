package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/registry"
	"github.com/value-quest/igor/internal/runner"
	"github.com/value-quest/igor/internal/supervisor"
)

func (a *App) cmdSetup() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "One-time setup: DNS for *.test, front proxy, local CA, and PHP CLI shims",
		RunE: func(cmd *cobra.Command, args []string) error {
			a.UI.Intro("igor setup")

			if err := a.Platform.SetupEnvironment(); err != nil {
				return err
			}

			for _, d := range []string{a.Cfg.IgorHome, a.Cfg.SitesDir, a.Cfg.LogsDir, a.Cfg.IniDir} {
				if err := a.Cfg.EnsureDir(d); err != nil {
					return err
				}
			}
			if err := a.Registry.Touch(); err != nil {
				return err
			}
			if err := a.Caddy.EnsureDefaultIni(); err != nil {
				return err
			}
			if err := a.Caddy.EnsureFrontCaddyfile(); err != nil {
				return err
			}

			proxyBin, err := a.Caddy.ProxyBin()
			if err != nil {
				return err
			}
			if err := a.Platform.EnsureProxyCapabilities(proxyBin); err != nil {
				return err
			}

			if err := a.UI.Spin("Starting front proxy...", func() error {
				if _, err := a.Caddy.WriteProxyPlist(); err != nil {
					return err
				}
				if err := a.Sup.Stop("proxy"); err != nil {
					return err
				}
				if err := a.Sup.Start("proxy"); err != nil {
					return err
				}
				if a.UI.Interactive {
					time.Sleep(2 * time.Second)
				}
				return nil
			}); err != nil {
				return err
			}

			a.UI.Info("Creating PHP CLI shims")
			shims, err := a.Shims.CreateShims()
			if err != nil {
				return err
			}
			a.UI.Info(fmt.Sprintf("Shims in %s: %s", a.Cfg.ShimDir, strings.Join(shims, " ")))
			if !a.Shims.IsOnPath() {
				a.UI.Callout("Add shims to PATH", []string{
					"Put them first on PATH in your shell profile (e.g. ~/.zshrc or ~/.bashrc):",
					`  export PATH="$HOME/.igor/bin:$PATH"`,
				}, false)
			}

			a.Platform.TrustCA(proxyBin)

			a.UI.Outro(`Done! Make sure your shell loads direnv (e.g. eval "$(direnv hook zsh)" or bash). Then, in a project: igor add <name>`)
			return nil
		},
	}
}

func (a *App) cmdAdd() *cobra.Command {
	var phpFlag, portFlag, docrootFlag string
	cmd := &cobra.Command{
		Use:   "add [<name>]",
		Short: "Serve the current project as https://<name>.test",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			if resolved, err := filepath.EvalSymlinks(dir); err == nil {
				dir = resolved
			}

			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if name == "" {
				if !a.UI.Interactive {
					return fmt.Errorf("usage: igor add <name> [--php 8.x] [--port N] [--docroot DIR]  (e.g. igor add portal.valuequest)")
				}
				name, err = a.UI.Input("Site name", "portal.valuequest", strings.ToLower(filepath.Base(dir)))
				if err != nil || name == "" {
					return fmt.Errorf("cancelled")
				}
			}

			name = config.StripTLD(name, a.Cfg.TLD)
			if err := config.ValidateName(name); err != nil {
				return err
			}
			if _, err := os.Stat(a.Cfg.Caddyfile); err != nil {
				return fmt.Errorf("run 'igor setup' first")
			}
			if existing, _ := a.Registry.Get(name); existing != nil {
				return fmt.Errorf("'%s' already exists (igor rm %s)", name, name)
			}

			docroot := docrootFlag
			if docroot == "" {
				if info, err := os.Stat(filepath.Join(dir, "public")); err == nil && info.IsDir() {
					docroot = "public"
				} else {
					docroot = "."
				}
			}
			docroot, err = config.NormalizeDocroot(docroot, dir)
			if err != nil {
				return err
			}

			if byDir, _ := a.Registry.FindByDir(dir, ""); byDir != nil {
				return fmt.Errorf("this folder is already served as '%s'", byDir.Name)
			}

			php := phpFlag
			from := "--php"
			if php == "" {
				if v := a.Runtimes.ReadPHPVersionFile(dir); v != "" {
					php = v
					from = ".php-version"
				}
			}
			if php == "" {
				if v := a.Runtimes.ComposerPlatformPHP(dir); v != "" {
					php = v
					from = "composer.json (config.platform.php)"
				}
			}
			if php == "" {
				v, err := a.Runtimes.NewestPHP()
				if err != nil {
					return err
				}
				php = v
				from = "newest installed"
			}

			if !a.Runtimes.IsInstalled(php) {
				return fmt.Errorf("PHP %s (from %s) is not installed in %s/php%s - run 'igor install', or pick one with --php 8.x", php, from, a.Cfg.PHPRoot, php)
			}

			if err := os.WriteFile(filepath.Join(dir, ".php-version"), []byte(php+"\n"), 0o644); err != nil {
				return err
			}

			var port int
			if portFlag != "" {
				p, err := strconv.Atoi(portFlag)
				if err != nil {
					return fmt.Errorf("--port must be a number")
				}
				port = p
			} else {
				port, err = a.Registry.NextPort()
				if err != nil {
					return err
				}
			}
			for a.Run.PortInUse(strconv.Itoa(port)) {
				port++
			}

			envrcPath := filepath.Join(dir, ".envrc")
			envrcContent, _ := os.ReadFile(envrcPath)
			if !strings.Contains(string(envrcContent), "igor") {
				snippet := "# igor: PHP runtime from .php-version\nPATH_add \"$HOME/frankenphp/php$(tr -d '[:space:]' < .php-version)/bin\"\nexport PHP_INI_SCAN_DIR=\"$HOME/.igor/php\"\n"
				f, err := os.OpenFile(envrcPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err == nil {
					_, _ = f.WriteString(snippet)
					f.Close()
				}
				if _, err := a.Run.LookPath("direnv"); err == nil {
					a.Run.RunWith(runner.Opts{Dir: dir}, "direnv", "allow")
				}
			}

			entry := registry.Entry{
				Name:    name,
				Port:    strconv.Itoa(port),
				Type:    "php",
				PHP:     php,
				Dir:     dir,
				Docroot: docroot,
			}
			if err := a.Registry.Add(entry); err != nil {
				return err
			}
			if _, err := a.Caddy.WriteSite(entry); err != nil {
				return err
			}
			if err := a.Caddy.WriteRoute(name, entry.Port); err != nil {
				return err
			}
			if err := a.Sup.Start(name); err != nil {
				return err
			}
			_ = a.Platform.SyncHosts()
			if err := a.ReloadProxyOrWarn(); err != nil {
				return err
			}

			if _, err := os.Stat(filepath.Join(dir, "Procfile.dev")); err == nil {
				a.UI.Info("Workers from Procfile.dev:")
				changes, err := a.Workers.SyncWorkers(entry, false)
				if err != nil {
					return err
				}
				for _, ch := range changes {
					a.UI.Note("  " + ch)
				}
			}

			if err := a.Workers.WriteWatcher(entry); err != nil {
				return err
			}

			a.UI.Callout(fmt.Sprintf("https://%s.%s", name, a.Cfg.TLD), []string{
				fmt.Sprintf("https://%s.%s  ->  http://127.0.0.1:%d  (PHP %s from %s, serving %s/)", name, a.Cfg.TLD, port, php, from, docroot),
				"Pinned in .php-version - add .php-version and .envrc to git if the team should share it.",
			}, false)
			return nil
		},
	}

	cmd.Flags().StringVar(&phpFlag, "php", "", "PHP version (e.g. 8.4 or 8.5)")
	cmd.Flags().StringVar(&portFlag, "port", "", "Internal backend port")
	cmd.Flags().StringVar(&docrootFlag, "docroot", "", "Document root relative to project root")
	return cmd
}

func (a *App) cmdProxy() *cobra.Command {
	return &cobra.Command{
		Use:   "proxy <name> <port>",
		Short: "Route https://<name>.test to a service you run yourself",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("usage: igor proxy <name> <port>")
			}
			name := config.StripTLD(args[0], a.Cfg.TLD)
			if err := config.ValidateName(name); err != nil {
				return err
			}
			if _, err := strconv.Atoi(args[1]); err != nil {
				return fmt.Errorf("port must be a number")
			}
			if existing, _ := a.Registry.Get(name); existing != nil {
				return fmt.Errorf("'%s' already exists", name)
			}
			if err := a.Registry.Add(registry.Entry{
				Name: name,
				Port: args[1],
				Type: "proxy",
				PHP:  "-",
				Dir:  "-",
			}); err != nil {
				return err
			}
			if err := a.Caddy.WriteRoute(name, args[1]); err != nil {
				return err
			}
			_ = a.Platform.SyncHosts()
			if err := a.ReloadProxyOrWarn(); err != nil {
				return err
			}
			a.UI.Info(fmt.Sprintf("https://%s.%s  ->  http://127.0.0.1:%s  (you run the service yourself)", name, a.Cfg.TLD, args[1]))
			return nil
		},
	}
}

func (a *App) cmdService() *cobra.Command {
	svc := &cobra.Command{
		Use:   "service",
		Short: "Manage background services (Redis, Mailpit, S3, ...)",
	}

	var routeFlag string
	var envFlags []string

	addCmd := &cobra.Command{
		Use:   "add <name> [--route PORT] [--env KEY=VALUE]... -- <command...>",
		Short: "Add or update a background service",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("usage: igor service add <name> [--route PORT] [--env KEY=VALUE]... -- <command...>")
			}
			name := args[0]
			cmdArgs := args[1:]

			if err := config.ValidateName(name); err != nil {
				return err
			}
			route := "-"
			if routeFlag != "" {
				if _, err := strconv.Atoi(routeFlag); err != nil {
					return fmt.Errorf("--route needs a port number")
				}
				route = routeFlag
			}

			var envs []supervisor.EnvVar
			for _, pair := range envFlags {
				idx := strings.IndexByte(pair, '=')
				if idx <= 0 {
					return fmt.Errorf("--env needs KEY=VALUE")
				}
				envs = append(envs, supervisor.EnvVar{Key: pair[:idx], Value: pair[idx+1:]})
			}

			if _, err := os.Stat(a.Cfg.Caddyfile); err != nil {
				return fmt.Errorf("run 'igor setup' first")
			}

			if existing, _ := a.Registry.Get(name); existing != nil && existing.Type != "service" {
				return fmt.Errorf("'%s' already exists as a %s (igor rm %s)", name, existing.Type, name)
			}

			exe := cmdArgs[0]
			if !strings.Contains(exe, "/") {
				found, err := a.Run.LookPath(exe, a.Platform.ExtraLookPaths()...)
				if err != nil {
					return fmt.Errorf("'%s' not found - install it first (e.g. %s)", exe, a.Platform.InstallHint(exe))
				}
				exe = found
			}
			info, err := os.Stat(exe)
			if err != nil || info.Mode()&0o111 == 0 {
				return fmt.Errorf("%s is not executable", exe)
			}

			dataDir := a.Cfg.ServiceDataDir(name)
			if err := a.Cfg.EnsureDir(dataDir); err != nil {
				return err
			}

			if err := a.Registry.Upsert(registry.Entry{
				Name: name,
				Port: route,
				Type: "service",
				PHP:  "-",
				Dir:  dataDir,
			}); err != nil {
				return err
			}

			progArgs := append([]string{exe}, cmdArgs[1:]...)
			changed, err := a.Sup.WriteUnit(name, dataDir, a.Platform.ServicePATH(), progArgs, envs, nil)
			if err != nil {
				return err
			}

			var offset int64
			if st, err := os.Stat(a.Cfg.LogPath(name)); err == nil {
				offset = st.Size()
			}
			if changed {
				if err := a.Sup.Stop(name); err != nil {
					return err
				}
			}
			if err := a.Sup.Start(name); err != nil {
				return err
			}

			if route != "-" {
				if err := a.Caddy.WriteRoute(name, route); err != nil {
					return err
				}
			} else {
				a.Caddy.RemoveRoute(name)
			}
			_ = a.Platform.SyncHosts()
			if err := a.ReloadProxyOrWarn(); err != nil {
				return err
			}

			lines := []string{
				fmt.Sprintf("%s: %s", name, strings.Join(progArgs, " ")),
				fmt.Sprintf("  data: %s   log: igor logs %s", dataDir, name),
			}
			if route != "-" {
				lines = append(lines, fmt.Sprintf("  https://%s.%s  ->  http://127.0.0.1:%s", name, a.Cfg.TLD, route))
			}
			a.UI.Callout(name, lines, false)

			wait := 2 * time.Second
			if a.Run.IsFake() {
				wait = 0
			}
			return a.Sup.CheckAlive(name, offset, wait)
		},
	}
	addCmd.Flags().StringVar(&routeFlag, "route", "", "Serve web UI on https://<name>.test -> 127.0.0.1:<port>")
	addCmd.Flags().StringArrayVar(&envFlags, "env", nil, "Environment variable KEY=VALUE (repeatable)")

	rmCmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a service",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return a.removeEntry(name)
		},
	}

	svc.AddCommand(addCmd, rmCmd)
	return svc
}

func (a *App) cmdRm() *cobra.Command {
	return &cobra.Command{
		Use:   "rm [<name>]",
		Short: "Remove a site, service or route (project files and service data are not touched)",
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
					name, err = a.UI.Select("Which site, service, or route should be removed?", opts)
					if err != nil {
						return fmt.Errorf("cancelled")
					}
				}
			}
			return a.removeEntry(name)
		},
	}
}

func (a *App) removeEntry(name string) error {
	e, err := a.Registry.Get(name)
	if err != nil {
		return err
	}
	if e == nil {
		return fmt.Errorf("no site, service or route '%s'", name)
	}
	for _, w := range a.Workers.SiteWorkers(name) {
		_ = a.Sup.Remove(w)
	}
	_ = a.Sup.Remove(a.Cfg.WatcherName(name))
	_ = a.Sup.Remove(name)
	a.Caddy.RemoveRoute(name)
	_ = os.Remove(a.Cfg.BackendCaddyfilePath(name))
	if err := a.Registry.Remove(name); err != nil {
		return err
	}
	_ = a.Platform.SyncHosts()
	if err := a.ReloadProxyOrWarn(); err != nil {
		return err
	}
	if e.Type == "service" {
		a.UI.Info(fmt.Sprintf("Removed %s (data left in %s)", name, a.Cfg.ServiceDataDir(name)))
	} else {
		a.UI.Info(fmt.Sprintf("Removed %s (project files, .php-version and .envrc left as they are)", name))
	}
	return nil
}

func (a *App) cmdWorkers() *cobra.Command {
	return &cobra.Command{
		Use:   "workers [<name>]",
		Short: "Sync and start Procfile.dev workers for a site",
		RunE: func(cmd *cobra.Command, args []string) error {
			site := ""
			if len(args) > 0 {
				site = args[0]
			}
			if site == "" {
				cwd, _ := os.Getwd()
				if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
					cwd = resolved
				}
				if found, _ := a.Registry.FindByDir(cwd, "php"); found != nil {
					site = found.Name
				}
			}
			if site == "" {
				return fmt.Errorf("this folder is not an igor site - run 'igor add <name>' first, or 'igor workers <name>'")
			}

			entry, err := a.Registry.RequirePHPSite(site)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(entry.Dir, "Procfile.dev")); err != nil {
				a.UI.Warn(fmt.Sprintf("No Procfile.dev in %s - removing any workers of %s", entry.Dir, site))
			}

			changes, err := a.Workers.SyncWorkers(*entry, false)
			if err != nil {
				return err
			}
			for _, ch := range changes {
				a.UI.Note("  " + ch)
			}

			var rows [][]string
			for _, w := range a.Workers.SiteWorkers(site) {
				state := "stopped"
				if a.Sup.IsRunning(w) {
					state = "running"
				}
				rows = append(rows, []string{a.Cfg.DisplayAgentName(w), state})
			}
			if len(rows) > 0 {
				a.UI.Table([]string{"WORKER", "STATE"}, rows)
			}
			return nil
		},
	}
}
