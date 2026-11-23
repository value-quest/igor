package supervisor

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/value-quest/igor/internal/config"
)

type EnvVar struct {
	Key   string
	Value string
}

// Manager abstracts process supervision (launchd on macOS, systemd --user on Linux/WSL2).
type Manager interface {
	WriteUnit(name, workdir, path string, args []string, extraEnv []EnvVar, watchPaths []string) (bool, error)
	UnitPath(agent string) string
	Exists(agent string) bool
	SiteWorkers(site string) []string
	CommandString(agent string) string
	IsRunning(agent string) bool
	IsAlive(agent string) bool
	Start(agent string) error
	Stop(agent string) error
	Disable(agent string) error
	Remove(agent string) error
	Bounce(agent string, changed, force bool) (bool, error)
	CheckAlive(name string, logOffset int64, wait time.Duration) error
}

func checkAliveWith(cfg *config.Config, supervisorName string, isAlive func(string) bool, name string, logOffset int64, wait time.Duration) error {
	if wait > 0 {
		time.Sleep(wait)
	}
	if isAlive(name) {
		return nil
	}

	logFile := cfg.LogPath(name)
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
		"%s is not running - it exited right after starting. Its log:\n%s\n%s keeps retrying every 10s. Fix the command and run 'igor service add %s ...' again,\nor remove it with 'igor rm %s'.",
		name, excerpt, supervisorName, name, name,
	)
}
