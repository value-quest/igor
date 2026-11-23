//go:build linux

package supervisor

import (
	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/runner"
)

func New(cfg *config.Config, run runner.Runner) Manager {
	return NewSystemd(cfg, run)
}
