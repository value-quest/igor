package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	FirstPort   = 8101
	LabelPrefix = "dev.igor"
)

type Config struct {
	Home         string
	IgorHome     string
	IgorBin      string
	PHPRoot      string
	TLD          string
	SuiteRepo    string
	IgorRepo     string
	AgentsDir    string
	ResolverDir  string
	RegistryFile string
	Caddyfile    string
	SitesDir     string
	BackendsDir  string
	ShimDir      string
	LogsDir      string
	IniDir       string
	DataDir      string
	IsWatch      bool
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = envOr("HOME", "/tmp")
	}

	igorHome := envOr("IGOR_HOME", filepath.Join(home, ".igor"))
	phpRoot := envOr("IGOR_PHP_ROOT", filepath.Join(home, "frankenphp"))
	tld := envOr("IGOR_TLD", "test")
	suiteRepo := envOr("IGOR_SUITE_REPO", "value-quest/franken-php")
	igorRepo := envOr("IGOR_REPO", "value-quest/igor")
	isWatch := os.Getenv("IGOR_WATCH") != ""

	exe, err := os.Executable()
	if err == nil {
		if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			exe = resolved
		}
	}

	return New(home, igorHome, exe, phpRoot, tld, suiteRepo, igorRepo, filepath.Join(home, "Library", "LaunchAgents"), isWatch)
}

func New(home, igorHome, igorBin, phpRoot, tld, suiteRepo, igorRepo, agentsDir string, isWatch bool) *Config {
	home = strings.TrimRight(home, "/")
	igorHome = strings.TrimRight(igorHome, "/")
	phpRoot = strings.TrimRight(phpRoot, "/")
	agentsDir = strings.TrimRight(agentsDir, "/")

	return &Config{
		Home:         home,
		IgorHome:     igorHome,
		IgorBin:      igorBin,
		PHPRoot:      phpRoot,
		TLD:          tld,
		SuiteRepo:    suiteRepo,
		IgorRepo:     igorRepo,
		AgentsDir:    agentsDir,
		ResolverDir:  "/etc/resolver",
		RegistryFile: filepath.Join(igorHome, "sites.tsv"),
		Caddyfile:    filepath.Join(igorHome, "Caddyfile"),
		SitesDir:     filepath.Join(igorHome, "sites"),
		BackendsDir:  filepath.Join(igorHome, "backends"),
		ShimDir:      filepath.Join(igorHome, "bin"),
		LogsDir:      filepath.Join(igorHome, "logs"),
		IniDir:       filepath.Join(igorHome, "php"),
		DataDir:      filepath.Join(igorHome, "data"),
		IsWatch:      isWatch,
	}
}

func (c *Config) PHPBin(version string) string {
	return filepath.Join(c.PHPRoot, "php"+version, "bin")
}

func (c *Config) Label(agent string) string {
	return LabelPrefix + "." + agent
}

func (c *Config) PlistPath(agent string) string {
	return filepath.Join(c.AgentsDir, c.Label(agent)+".plist")
}

func (c *Config) LogPath(agent string) string {
	return filepath.Join(c.LogsDir, agent+".log")
}

func (c *Config) SiteRoutePath(name string) string {
	return filepath.Join(c.SitesDir, name+".caddy")
}

func (c *Config) BackendCaddyfilePath(name string) string {
	return filepath.Join(c.BackendsDir, name+".Caddyfile")
}

func (c *Config) ServiceDataDir(name string) string {
	return filepath.Join(c.DataDir, name)
}

func (c *Config) WatcherName(site string) string {
	return site + "~watch"
}

func (c *Config) ResolverPath() string {
	dir := c.ResolverDir
	if dir == "" {
		dir = "/etc/resolver"
	}
	return filepath.Join(dir, c.TLD)
}

func (c *Config) AgentName(input string) string {
	if strings.HasSuffix(input, ":watch") {
		return strings.TrimSuffix(input, ":watch") + "~watch"
	}
	return strings.Replace(input, ":", "@", 1)
}

func (c *Config) DisplayAgentName(agent string) string {
	if strings.HasSuffix(agent, "~watch") {
		return strings.TrimSuffix(agent, "~watch") + ":watch"
	}
	return strings.Replace(agent, "@", ":", 1)
}

func (c *Config) Domain() string {
	return fmt.Sprintf("gui/%d", os.Geteuid())
}

func (c *Config) EnsureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}

var nameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

func ValidateName(name string) error {
	if !nameRegexp.MatchString(name) {
		return fmt.Errorf("invalid name '%s' (lowercase letters, digits, dashes; dots between parts)", name)
	}
	if name == "proxy" {
		return fmt.Errorf("'proxy' is reserved for the front proxy")
	}
	return nil
}

func StripTLD(name, tld string) string {
	return strings.TrimSuffix(name, "."+tld)
}

func NormalizeDocroot(docroot, projectDir string) (string, error) {
	docroot = strings.TrimRight(docroot, "/")
	docroot = strings.TrimPrefix(docroot, "./")
	if docroot == "" {
		docroot = "."
	}
	if strings.HasPrefix(docroot, "/") || strings.HasPrefix(docroot, "..") || strings.Contains(docroot, "/../") || strings.HasSuffix(docroot, "/..") {
		return "", fmt.Errorf("--docroot must be a folder inside the project (e.g. public, web, .)")
	}
	full := projectDir
	if docroot != "." {
		full = filepath.Join(projectDir, docroot)
	}
	info, err := os.Stat(full)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("no %s/ in %s - run this in the project root", docroot, projectDir)
	}
	return docroot, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
