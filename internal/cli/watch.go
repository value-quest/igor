package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/value-quest/igor/internal/worker"
)

func (a *App) cmdWatch() *cobra.Command {
	return &cobra.Command{
		Use:    "watch",
		Short:  "Watch all registered sites for .php-version, composer.json, Procfile.dev, and git branch changes",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Cfg.IsWatch = true
			a.UI.Interactive = false
			return a.runWatchLoop()
		},
	}
}

func siteFingerprint(dir string) string {
	var b strings.Builder
	for _, rel := range []string{".php-version", "composer.json", "Procfile.dev"} {
		p := filepath.Join(dir, rel)
		if info, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", rel, info.ModTime().UnixNano(), info.Size())
		} else {
			fmt.Fprintf(&b, "%s:missing;", rel)
		}
	}
	if headPath, _ := worker.GitHeadPath(dir); headPath != "" {
		if raw, err := os.ReadFile(headPath); err == nil {
			fmt.Fprintf(&b, "HEAD:%s;", strings.TrimSpace(string(raw)))
		}
	}
	return b.String()
}

func (a *App) runWatchLoop() error {
	fmt.Fprintf(a.UI.Out, "--- %s igor watch started\n", time.Now().Format("2006-01-02 15:04:05"))

	var mu sync.Mutex
	fingerprints := map[string]string{}
	inProgress := map[string]bool{}

	// Seed initial fingerprints and branch states for all registered sites
	if entries, err := a.Registry.All(); err == nil {
		for _, e := range entries {
			if e.Type != "php" {
				continue
			}
			worker.SeedBranchState(a.Cfg, e.Name, e.Dir)
			fingerprints[e.Name] = siteFingerprint(e.Dir)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			fmt.Fprintf(a.UI.Out, "--- %s igor watch stopped\n", time.Now().Format("2006-01-02 15:04:05"))
			return nil
		case <-ticker.C:
			entries, err := a.Registry.All()
			if err != nil {
				continue
			}
			seen := map[string]bool{}
			for _, e := range entries {
				if e.Type != "php" {
					continue
				}
				seen[e.Name] = true
				fp := siteFingerprint(e.Dir)

				mu.Lock()
				prev, known := fingerprints[e.Name]
				busy := inProgress[e.Name]
				if !known {
					worker.SeedBranchState(a.Cfg, e.Name, e.Dir)
					fingerprints[e.Name] = fp
					mu.Unlock()
					continue
				}
				if fp == prev || busy {
					mu.Unlock()
					continue
				}
				fingerprints[e.Name] = fp
				inProgress[e.Name] = true
				mu.Unlock()

				siteName := e.Name
				siteDir := e.Dir
				go func() {
					defer func() {
						mu.Lock()
						fingerprints[siteName] = siteFingerprint(siteDir)
						delete(inProgress, siteName)
						mu.Unlock()
					}()
					fmt.Fprintf(a.UI.Out, "--- %s [%s] .php-version, composer.json, Procfile.dev or git branch changed\n", time.Now().Format("2006-01-02 15:04:05"), siteName)
					if err := a.RefreshSite(siteName, false); err != nil {
						a.UI.Error(err.Error())
						a.Notify(err.Error())
						return
					}
					if err := a.ReloadProxyOrWarn(); err != nil {
						a.UI.Error(err.Error())
					}
				}()
			}

			mu.Lock()
			for name := range fingerprints {
				if !seen[name] {
					delete(fingerprints, name)
				}
			}
			mu.Unlock()
		}
	}
}
