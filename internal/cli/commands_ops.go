package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/value-quest/igor/internal/updater"
)

func (a *App) cmdInstall() *cobra.Command {
	return a.suiteCommand("install", "Install the FrankenPHP suite (every PHP version in the latest release)")
}

func (a *App) cmdUpdate() *cobra.Command {
	return a.suiteCommand("update", "Install the latest FrankenPHP suite release, then restart everything on the new binaries")
}

func (a *App) suiteCommand(name, short string) *cobra.Command {
	var checkFlag, forceFlag bool
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			var rel *updater.SuiteRelease
			if err := a.UI.Spin(fmt.Sprintf("Checking latest release of %s...", a.Cfg.SuiteRepo), func() error {
				var err error
				rel, err = a.Updater.FetchLatestSuite()
				return err
			}); err != nil {
				return err
			}

			fpSuffix := ""
			if rel.FrankenPHP != "" {
				fpSuffix = ", FrankenPHP " + rel.FrankenPHP
			}
			a.UI.Info(fmt.Sprintf("Latest suite release: %s (%s)%s", rel.Tag, a.Cfg.SuiteRepo, fpSuffix))
			if !rel.HasVersionsJSON {
				a.UI.Note("  (no versions.json in this release - only a new tag counts as an update)")
			}

			plan := a.Updater.PlanUpdate(rel, forceFlag)
			a.UI.Table([]string{"PHP", "INSTALLED", "AVAILABLE"}, plan.Rows)

			for _, v := range plan.Dropped {
				a.UI.Warn(fmt.Sprintf("PHP %s is no longer shipped: switch its projects to another version, then 'igor uninstall %s'.", v, v))
			}

			if len(plan.Pending) == 0 {
				if name == "install" {
					a.UI.Outro("Everything is installed and up to date.")
				} else {
					a.UI.Outro("Up to date.")
				}
				return nil
			}

			if checkFlag {
				a.UI.Note(fmt.Sprintf("Run 'igor %s' to install PHP %s.", name, strings.Join(plan.Pending, " ")))
				return nil
			}

			tmpDir, err := os.MkdirTemp("", "igor-update.*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmpDir)

			for _, v := range plan.Pending {
				var got, note string
				if err := a.UI.Spin(fmt.Sprintf("PHP %s: downloading & installing %s...", v, rel.Tag), func() error {
					var err error
					got, note, err = a.Updater.InstallVersion(v, rel, tmpDir)
					return err
				}); err != nil {
					return err
				}
				if note != "" {
					a.UI.Warn("  note: " + note)
				}
				fpDisp := rel.FrankenPHP
				if fpDisp == "" {
					fpDisp = "?"
				}
				a.UI.Info(fmt.Sprintf("  PHP %s, FrankenPHP %s", got, fpDisp))
			}

			if _, err := os.Stat(a.Cfg.Caddyfile); err != nil {
				a.UI.Outro("Installed. Next: igor setup")
				return nil
			}

			a.UI.Info("Restarting everything on the new binaries")
			return a.runRefresh("", true)
		},
	}

	cmd.Flags().BoolVar(&checkFlag, "check", false, "Show installed vs. available versions without changing anything")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Reinstall even if already up to date")
	return cmd
}

var minorVerRe = regexp.MustCompile(`^\d+\.\d+$`)

func (a *App) cmdUninstall() *cobra.Command {
	var yesFlag bool
	cmd := &cobra.Command{
		Use:   "uninstall [<8.x>]",
		Short: "Remove a PHP runtime from ~/frankenphp (not igor itself)",
		RunE: func(cmd *cobra.Command, args []string) error {
			version := ""
			if len(args) > 0 {
				version = args[0]
			}
			if version == "" {
				installed := a.Runtimes.InstalledVersions()
				if !a.UI.Interactive || len(installed) == 0 {
					return fmt.Errorf("usage: igor uninstall <8.x> [--yes]   removes a PHP runtime from %s (not igor)", a.Cfg.PHPRoot)
				}
				var opts []huh.Option[string]
				for _, v := range installed {
					label := "PHP " + v
					if patch := a.Runtimes.InstalledPHPPatch(v); patch != "" {
						label += " (" + patch + ")"
					}
					opts = append(opts, huh.NewOption(label, v))
				}
				var err error
				version, err = a.UI.Select("Which PHP runtime should be removed?", opts)
				if err != nil {
					return fmt.Errorf("cancelled")
				}
			}

			if !minorVerRe.MatchString(version) {
				return fmt.Errorf("usage: igor uninstall <8.x> [--yes]   removes a PHP runtime from %s (not igor)", a.Cfg.PHPRoot)
			}

			targetDir := filepath.Join(a.Cfg.PHPRoot, "php"+version)
			if _, err := os.Stat(targetDir); err != nil {
				return fmt.Errorf("PHP %s is not installed in %s", version, a.Cfg.PHPRoot)
			}

			users, _ := a.Registry.SitesUsingPHP(version)
			if len(users) > 0 {
				var lines []string
				for _, u := range users {
					lines = append(lines, fmt.Sprintf("  %s  (%s)", u.Name, u.Dir))
				}
				return fmt.Errorf("PHP %s is still used by:\n%s\nChange .php-version in those projects to another version, run 'igor refresh', then try again.", version, strings.Join(lines, "\n"))
			}

			var remaining []string
			for _, v := range a.Runtimes.InstalledVersions() {
				if v != version {
					remaining = append(remaining, v)
				}
			}
			if _, err := os.Stat(a.Cfg.Caddyfile); err == nil && len(remaining) == 0 {
				return fmt.Errorf("PHP %s is the only runtime left - the front proxy needs one (install another with 'igor install' first)", version)
			}

			backups := a.Runtimes.BackupsFor(version)
			patch := a.Runtimes.InstalledPHPPatch(version)
			lines := []string{fmt.Sprintf("  %s  (PHP %s)", targetDir, patch)}
			for _, b := range backups {
				lines = append(lines, "  "+b)
			}
			a.UI.Callout("This removes:", lines, true)

			if !yesFlag {
				if !a.UI.Interactive {
					return fmt.Errorf("not a terminal - pass --yes to confirm")
				}
				ok, err := a.UI.Confirm(fmt.Sprintf("Remove PHP %s?", version), false)
				if err != nil || !ok {
					return fmt.Errorf("cancelled")
				}
			}

			if err := os.RemoveAll(targetDir); err != nil {
				return err
			}
			for _, b := range backups {
				if strings.HasPrefix(b, filepath.Join(a.Cfg.PHPRoot, "php"+version+".backup.")) {
					_ = os.RemoveAll(b)
				}
			}
			a.UI.Info("Removed PHP " + version)

			if _, err := os.Stat(a.Cfg.Caddyfile); err == nil {
				a.UI.Info("Moving the front proxy and shims to the remaining runtimes")
				return a.runRefresh("", false)
			}
			_, err := a.Shims.CreateShims()
			return err
		},
	}

	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "Skip confirmation prompt")
	return cmd
}

func (a *App) cmdDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check dnsmasq, resolver, proxy, Herd conflicts, PHP runtimes, shims, and registered sites",
		RunE: func(cmd *cobra.Command, args []string) error {
			a.UI.Intro("igor doctor")

			var (
				brewPath       string
				direnvPath     string
				brewPrefix     string
				hasDnsmasq     bool
				dnsmasqRunning bool
				proxyUp        bool
				herdRunning    bool
				wg             sync.WaitGroup
			)

			wg.Add(7)
			go func() { defer wg.Done(); brewPath, _ = a.Run.LookPath("brew") }()
			go func() { defer wg.Done(); direnvPath, _ = a.Run.LookPath("direnv") }()
			go func() { defer wg.Done(); brewPrefix = strings.TrimSpace(a.Run.Run("brew", "--prefix").Stdout) }()
			go func() { defer wg.Done(); hasDnsmasq = a.Run.Run("brew", "list", "dnsmasq").OK() }()
			go func() { defer wg.Done(); dnsmasqRunning = a.Run.Run("pgrep", "-x", "dnsmasq").OK() }()
			go func() { defer wg.Done(); proxyUp = a.Launchd.IsRunning("proxy") }()
			go func() { defer wg.Done(); herdRunning = a.Run.Run("pgrep", "-qf", "Herd").OK() }()
			wg.Wait()

			var rows [][]string
			failures, warnings := 0, 0

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
					hasRule = strings.Contains(string(raw), fmt.Sprintf("address=/.%s/127.0.0.1", a.Cfg.TLD))
				}
			}
			switch {
			case hasDnsmasq && hasRule && dnsmasqRunning:
				rows = append(rows, []string{"dnsmasq", "OK", fmt.Sprintf("*.%s -> 127.0.0.1 (%s)", a.Cfg.TLD, dnsmasqConf)})
			case hasDnsmasq && hasRule:
				rows = append(rows, []string{"dnsmasq", "FAIL", "Configured, but not running (run 'sudo brew services restart dnsmasq' or 'igor setup')"})
				failures++
			case hasDnsmasq:
				rows = append(rows, []string{"dnsmasq", "WARN", fmt.Sprintf("Installed, but *.%s rule missing (run 'igor setup')", a.Cfg.TLD)})
				warnings++
			default:
				rows = append(rows, []string{"dnsmasq", "FAIL", "Not installed (run 'igor setup')"})
				failures++
			}

			resolverFile := a.Cfg.ResolverPath()
			if raw, err := os.ReadFile(resolverFile); err == nil && strings.TrimSpace(string(raw)) == "nameserver 127.0.0.1" {
				rows = append(rows, []string{fmt.Sprintf("Resolver (%s)", resolverFile), "OK", "nameserver 127.0.0.1"})
			} else {
				rows = append(rows, []string{fmt.Sprintf("Resolver (%s)", resolverFile), "FAIL", "Missing or wrong (run 'igor setup')"})
				failures++
			}

			versions := a.Runtimes.InstalledVersions()
			if len(versions) > 0 {
				rows = append(rows, []string{"PHP runtimes", "OK", fmt.Sprintf("%s in %s", strings.Join(versions, ", "), a.Cfg.PHPRoot)})
			} else {
				rows = append(rows, []string{"PHP runtimes", "FAIL", fmt.Sprintf("None installed in %s (run 'igor install')", a.Cfg.PHPRoot)})
				failures++
			}

			shimInfo, shimErr := os.Stat(filepath.Join(a.Cfg.ShimDir, "igor-shim"))
			hasShim := shimErr == nil && shimInfo.Mode()&0o111 != 0
			onPath := a.Shims.IsOnPath()
			switch {
			case hasShim && onPath:
				rows = append(rows, []string{"CLI shims", "OK", a.Cfg.ShimDir + " is on PATH"})
			case hasShim:
				rows = append(rows, []string{"CLI shims", "WARN", a.Cfg.ShimDir + " exists, but is not on PATH"})
				warnings++
			default:
				rows = append(rows, []string{"CLI shims", "WARN", "Not generated yet (run 'igor shims')"})
				warnings++
			}

			_, caddyErr := os.Stat(a.Cfg.Caddyfile)
			switch {
			case caddyErr == nil && proxyUp:
				rows = append(rows, []string{"Front proxy", "OK", "Running (dev.igor.proxy)"})
			case caddyErr == nil:
				rows = append(rows, []string{"Front proxy", "WARN", "Stopped (run 'igor start')"})
				warnings++
			default:
				rows = append(rows, []string{"Front proxy", "FAIL", "Not configured (run 'igor setup')"})
				failures++
			}

			if herdRunning {
				rows = append(rows, []string{"Herd conflict", "WARN", "Herd appears to be running and may hold ports 80/443"})
				warnings++
			} else {
				rows = append(rows, []string{"Herd conflict", "OK", "No Herd process detected"})
			}

			var missingDirs, missingPHPs []string
			all, _ := a.Registry.All()
			for _, e := range all {
				if e.Type != "php" {
					continue
				}
				if info, err := os.Stat(e.Dir); err != nil || !info.IsDir() {
					missingDirs = append(missingDirs, e.Name)
				}
				if !a.Runtimes.IsInstalled(e.PHP) {
					missingPHPs = append(missingPHPs, fmt.Sprintf("%s (PHP %s)", e.Name, e.PHP))
				}
			}
			if len(missingDirs) == 0 && len(missingPHPs) == 0 {
				rows = append(rows, []string{"Registered sites", "OK", "All site folders and PHP versions are present"})
			} else {
				var probs []string
				if len(missingDirs) > 0 {
					probs = append(probs, "missing dirs: "+strings.Join(missingDirs, ", "))
				}
				if len(missingPHPs) > 0 {
					probs = append(probs, "missing PHP: "+strings.Join(missingPHPs, ", "))
				}
				rows = append(rows, []string{"Registered sites", "WARN", strings.Join(probs, "; ")})
				warnings++
			}

			a.UI.Table([]string{"CHECK", "STATUS", "DETAILS"}, rows)

			if failures > 0 {
				return fmt.Errorf("%d check(s) failed, %d warning(s)", failures, warnings)
			}
			if warnings > 0 {
				a.UI.Warn(fmt.Sprintf("All required checks passed, with %d warning(s).", warnings))
				return nil
			}
			a.UI.Outro("Everything looks healthy!")
			return nil
		},
	}
}

func (a *App) cmdSelfUpdate() *cobra.Command {
	var checkFlag, forceFlag bool
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update the igor binary from GitHub releases",
		RunE: func(cmd *cobra.Command, args []string) error {
			var rel *updater.GitHubRelease
			if err := a.UI.Spin(fmt.Sprintf("Checking latest igor release (%s)...", a.Cfg.IgorRepo), func() error {
				var err error
				rel, err = a.Updater.FetchGitHubRelease(a.Cfg.IgorRepo)
				return err
			}); err != nil {
				return err
			}

			latest := strings.TrimPrefix(rel.TagName, "v")
			a.UI.Info(fmt.Sprintf("Installed: v%s   Latest: %s (%s)", Version, rel.TagName, a.Cfg.IgorRepo))

			if !forceFlag && latest == Version {
				a.UI.Outro("igor is up to date.")
				return nil
			}
			if checkFlag {
				a.UI.Note(fmt.Sprintf("Run 'igor self-update' to update to %s.", rel.TagName))
				return nil
			}

			arch, err := updater.DetectArch()
			if err != nil {
				return err
			}
			assetName := "igor-darwin-" + arch
			dlURL := ""
			for _, asset := range rel.Assets {
				if asset.Name == assetName {
					dlURL = asset.BrowserDownloadURL
					break
				}
			}
			if dlURL == "" {
				return fmt.Errorf("release %s does not include asset '%s'", rel.TagName, assetName)
			}

			target := a.Cfg.IgorBin
			tmpFile := filepath.Join(filepath.Dir(target), ".igor-update.tmp")
			defer os.Remove(tmpFile)

			if err := a.UI.Spin(fmt.Sprintf("Downloading %s (%s)...", assetName, rel.TagName), func() error {
				return a.Updater.DownloadFile(dlURL, tmpFile)
			}); err != nil {
				return err
			}
			if err := os.Chmod(tmpFile, 0o755); err != nil {
				return err
			}
			if !a.Run.Run(tmpFile, "--version").OK() {
				return fmt.Errorf("downloaded binary failed self-check (--version)")
			}
			if err := os.Rename(tmpFile, target); err != nil {
				return fmt.Errorf("could not replace %s: %w", target, err)
			}

			a.UI.Outro(fmt.Sprintf("Updated igor to %s (%s).", rel.TagName, target))
			return nil
		},
	}

	cmd.Flags().BoolVar(&checkFlag, "check", false, "Check for a newer igor release without installing")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Reinstall even if already on the latest version")
	return cmd
}
