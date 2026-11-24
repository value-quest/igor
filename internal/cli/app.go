package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/value-quest/igor/internal/caddy"
	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/platform"
	"github.com/value-quest/igor/internal/registry"
	"github.com/value-quest/igor/internal/runner"
	"github.com/value-quest/igor/internal/runtime"
	"github.com/value-quest/igor/internal/shim"
	"github.com/value-quest/igor/internal/supervisor"
	"github.com/value-quest/igor/internal/ui"
	"github.com/value-quest/igor/internal/updater"
	"github.com/value-quest/igor/internal/worker"
)

var Version = "2.0.0"

type App struct {
	Cfg      *config.Config
	Run      runner.Runner
	UI       *ui.UI
	Registry *registry.Store
	Runtimes *runtime.Manager
	Sup      supervisor.Manager
	Platform *platform.Platform
	Caddy    *caddy.Manager
	Workers  *worker.Manager
	Shims    *shim.Manager
	Updater  *updater.Updater
}

func NewApp(cfg *config.Config, run runner.Runner, out, errOut io.Writer) *App {
	if cfg == nil {
		cfg = config.Default()
	}
	if run == nil {
		run = runner.New()
	}
	u := ui.New(out, errOut, cfg.IsWatch)
	reg := registry.New(cfg)
	rt := runtime.New(cfg, run)
	sup := supervisor.New(cfg, run)
	plat := platform.New(platform.Detect(), cfg, run, u, reg)
	cd := caddy.New(cfg, run, rt, sup)
	wk := worker.New(cfg, sup)
	sh := shim.New(cfg, rt)
	up := updater.New(cfg, run, rt)

	return &App{
		Cfg:      cfg,
		Run:      run,
		UI:       u,
		Registry: reg,
		Runtimes: rt,
		Sup:      sup,
		Platform: plat,
		Caddy:    cd,
		Workers:  wk,
		Shims:    sh,
		Updater:  up,
	}
}

func (a *App) Notify(msg string) {
	a.Platform.Notify(msg)
}

func (a *App) ReloadProxyOrWarn() error {
	reloaded, err := a.Caddy.ReloadProxy()
	if err != nil {
		return err
	}
	if !reloaded {
		a.UI.Warn("Front proxy is not running - run 'igor setup'")
	}
	return nil
}

func (a *App) Targets(name string) ([]string, error) {
	if strings.Contains(name, ":") {
		agent := a.Cfg.AgentName(name)
		if !a.Sup.Exists(agent) {
			return nil, fmt.Errorf("no worker '%s'", name)
		}
		return []string{agent}, nil
	}

	e, err := a.Registry.Get(name)
	if err != nil {
		return nil, err
	}
	if e != nil && e.Type == "service" {
		return []string{name}, nil
	}

	if _, err := a.Registry.RequirePHPSite(name); err != nil {
		return nil, err
	}
	list := append([]string{name}, a.Workers.SiteWorkers(name)...)
	w := a.Cfg.WatcherName(name)
	if a.Sup.Exists(w) {
		list = append(list, w)
	}
	return list, nil
}

func (a *App) Control(action, name string) error {
	list, err := a.Targets(name)
	if err != nil {
		return err
	}

	e, _ := a.Registry.Get(name)
	if action != "stop" && e != nil && e.Type == "php" {
		changed, err := a.Caddy.WriteSite(*e)
		if err != nil {
			return err
		}
		if changed {
			if err := a.Sup.Stop(name); err != nil {
				return err
			}
		}
	}

	for _, agent := range list {
		switch action {
		case "stop":
			if err := a.Sup.Disable(agent); err != nil {
				return err
			}
		case "start":
			if err := a.Sup.Start(agent); err != nil {
				return err
			}
		case "restart":
			if err := a.Sup.Stop(agent); err != nil {
				return err
			}
			if err := a.Sup.Start(agent); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) RefreshSite(name string, force bool) error {
	entry, err := a.Registry.RequirePHPSite(name)
	if err != nil {
		return err
	}
	a.UI.Info(name)

	if info, err := os.Stat(entry.Dir); err != nil || !info.IsDir() {
		a.UI.Warn(fmt.Sprintf("  %s is gone - skipped (igor rm %s)", entry.Dir, name))
		return nil
	}

	from := ".php-version"
	want := a.Runtimes.ReadPHPVersionFile(entry.Dir)
	if want == "" {
		want = a.Runtimes.ComposerPlatformPHP(entry.Dir)
		from = "composer.json"
	}

	if want == "" {
		a.UI.Warn(fmt.Sprintf("  no .php-version and no config.platform.php in composer.json - staying on PHP %s", entry.PHP))
		a.Notify(fmt.Sprintf("%s: no .php-version and no config.platform.php in this branch - still on PHP %s.", name, entry.PHP))
	} else if want != entry.PHP {
		if a.Runtimes.IsInstalled(want) {
			oldPHP := entry.PHP
			if err := a.Registry.UpdatePHP(name, want); err != nil {
				return err
			}
			entry.PHP = want
			a.UI.Note(fmt.Sprintf("  PHP %s -> %s (from %s)", oldPHP, want, from))
			a.Notify(fmt.Sprintf("%s now runs PHP %s (was %s)", name, want, oldPHP))
		} else {
			a.UI.Warn(fmt.Sprintf("  %s wants PHP %s, which is not installed - staying on %s", from, want, entry.PHP))
			a.Notify(fmt.Sprintf("%s: %s wants PHP %s, which is not installed - still on PHP %s. Run 'igor install'.", name, from, want, entry.PHP))
		}
	}

	siteChanged, err := a.Caddy.WriteSite(*entry)
	if err != nil {
		return err
	}
	if err := a.Caddy.WriteRoute(name, entry.Port); err != nil {
		return err
	}
	if bounced, err := a.Sup.Bounce(name, siteChanged, force); err != nil {
		return err
	} else if bounced {
		a.UI.Note("  restarted " + name)
	}

	changes, err := a.Workers.SyncWorkers(*entry, true)
	if err != nil {
		return err
	}
	for _, ch := range changes {
		a.UI.Note("  " + ch)
	}

	if force {
		for _, w := range a.Workers.SiteWorkers(name) {
			if bounced, err := a.Sup.Bounce(w, false, true); err != nil {
				return err
			} else if bounced {
				a.UI.Note("  restarted " + a.Cfg.DisplayAgentName(w))
			}
		}
	}

	if !a.Cfg.IsWatch {
		if err := a.Workers.WriteWatcher(*entry); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) RootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "igor",
		Short:         "Frankenstein's assistant: local PHP sites, workers, and services on macOS",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, args []string) {
			a.renderUsage()
		},
	}

	root.SetOut(a.UI.Out)
	root.SetErr(a.UI.Err)

	root.AddCommand(
		a.cmdSetup(),
		a.cmdAdd(),
		a.cmdProxy(),
		a.cmdService(),
		a.cmdRm(),
		a.cmdWorkers(),
		a.cmdStart(),
		a.cmdStop(),
		a.cmdRestart(),
		a.cmdList(),
		a.cmdLogs(),
		a.cmdInfo(),
		a.cmdReload(),
		a.cmdInstall(),
		a.cmdUpdate(),
		a.cmdUninstall(),
		a.cmdRefresh(),
		a.cmdShims(),
		a.cmdDoctor(),
		a.cmdSelfUpdate(),
	)

	return root
}

func (a *App) renderUsage() {
	tld := a.Cfg.TLD
	home := a.Cfg.IgorHome
	fmt.Fprintf(a.UI.Out, `Usage: igor <command> [args]

  setup                     one-time: dnsmasq for *.%s, front proxy, local CA
  add <name> [--php 8.x] [--port N] [--docroot DIR]
                            serve the current project as https://<name>.%s
                            (e.g. 'igor add portal.valuequest')
  proxy <name> <port>       route https://<name>.%s to a service you run
                            yourself (Reverb, Mailpit UI, Vite, ...)
  service add <name> [--route PORT] [--env KEY=VALUE]... -- <command...>
                            run a background service (Redis, Mailpit, S3, ...)
                            in %s/data/<name>; --route PORT also serves
                            its web UI as https://<name>.%s. Run it again to
                            change the command.
  rm <name>                 remove a site, service or route (project files and
                            service data are not touched)
  workers [<name>]          start the site's Procfile.dev processes (reverb,
                            queue, ...) and keep them running; rerun after
                            editing Procfile.dev
  start|stop|restart <name> control a site and its workers, or a service
                            (<name>:<worker> for a single worker); stopped
                            stays stopped after a restart of the Mac
  start | stop              everything: proxy, sites, workers and services
                            (e.g. to switch to Herd and back)
  list                      all sites, workers, services and routes with state
  logs <name>               follow a log (<name>:<worker>, or "proxy")
  logs --clear [<name>]     empty one log, or all of them
  info|status [<name>]      URLs, PHP version and .env hints for internal access
  reload                    reload the front proxy config
  install [--check] [--force]
                            install the FrankenPHP suite (every PHP version in
                            the latest release); same as update, use it first
  update [--check] [--force]
                            install the latest suite release, then restart
                            everything on the new binaries
  uninstall <8.x> [--yes]   remove a PHP runtime the suite no longer ships
  refresh [<name>] [--restart]
                            regenerate all configs (PHP from each .php-version,
                            Procfile.dev workers, routes, newest PHP for the
                            proxy) and restart what changed; --restart
                            restarts everything running (after a PHP update)
  shims                     (re)create php/composer/frankenphp shims in
                            %s/bin, plus php84, composer85, ... per
                            installed version (rerun after installing one)
  doctor                    check dnsmasq, resolver, proxy, Herd, runtimes,
                            shims and registered sites
  self-update [--check] [--force]
                            update the igor binary from GitHub releases
`, tld, tld, tld, home, tld, home)
}

func Execute() int {
	app := NewApp(nil, nil, os.Stdout, os.Stderr)
	if err := app.RootCmd().Execute(); err != nil {
		app.UI.Error(err.Error())
		app.Notify(err.Error())
		return 1
	}
	return 0
}
