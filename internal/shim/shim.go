package shim

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/value-quest/igor/internal/config"
	"github.com/value-quest/igor/internal/runtime"
)

type Manager struct {
	cfg      *config.Config
	runtimes *runtime.Manager
}

func New(cfg *config.Config, runtimes *runtime.Manager) *Manager {
	return &Manager{cfg: cfg, runtimes: runtimes}
}

const shimScript = `#!/bin/bash
# igor shim - php, composer, frankenphp from the shared FrankenPHP runtimes.
#   php84, composer85, frankenphp84 ...  fixed version
#   php, composer, frankenphp            nearest .php-version from $PWD upwards
#                                        (or composer.json config.platform.php),
#                                        else the newest installed runtime
set -eu
PHP_ROOT="${IGOR_PHP_ROOT:-$HOME/frankenphp}"
name=$(basename "$0")
tool=${name%%[0-9]*}
digits=${name#"$tool"}
v="" from=""

if [ -n "$digits" ]; then
    v="${digits:0:1}.${digits:1}"
else
    d=$PWD
    while :; do
        if [ -f "$d/.php-version" ]; then
            from="$d/.php-version"
            v=$(tr -d '[:space:]' < "$from" | sed -E 's/^php//; s/^([0-9]+\.[0-9]+).*/\1/')
            break
        fi
        if [ -f "$d/composer.json" ]; then
            # no .php-version: composer.json's config.platform.php, if set
            v=$(plutil -extract config.platform.php raw -o - "$d/composer.json" 2>/dev/null |
                sed -nE 's/^([0-9]+\.[0-9]+)(\.[0-9]+)?$/\1/p' || true)
            if [ -n "$v" ]; then from="$d/composer.json"; break; fi
        fi
        [ "$d" = / ] && break
        d=$(dirname "$d")
    done
    if [ -z "$v" ]; then
        v=$(ls -d "$PHP_ROOT"/php8.* 2>/dev/null | sed 's|.*/php||' | grep -E '^[0-9]+\.[0-9]+$' |
            sort -t. -k1,1nr -k2,2nr | head -1 || true)
    fi
fi

bin="$PHP_ROOT/php$v/bin"
if [ -z "$v" ] || [ ! -x "$bin/$tool" ]; then
    echo "igor: PHP ${v:-?} is not installed in $PHP_ROOT${from:+ (wanted by $from)}" >&2
    exit 127
fi

# Child processes (composer scripts, artisan, #!/usr/bin/env php) get the same version
export PATH="$bin:$PATH"
export PHP_INI_SCAN_DIR="${PHP_INI_SCAN_DIR:-${IGOR_HOME:-$HOME/.igor}/php}"
exec "$bin/$tool" "$@"
`

const linuxComposerExtract = `            if command -v jq >/dev/null 2>&1; then
                v=$(jq -r '.config.platform.php // empty' "$d/composer.json" 2>/dev/null |
                    sed -nE 's/^([0-9]+\.[0-9]+)(\.[0-9]+)?$/\1/p' || true)
            elif command -v python3 >/dev/null 2>&1; then
                v=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d.get("config",{}).get("platform",{}).get("php",""))' "$d/composer.json" 2>/dev/null |
                    sed -nE 's/^([0-9]+\.[0-9]+)(\.[0-9]+)?$/\1/p' || true)
            fi`

const darwinComposerExtract = `            v=$(plutil -extract config.platform.php raw -o - "$d/composer.json" 2>/dev/null |
                sed -nE 's/^([0-9]+\.[0-9]+)(\.[0-9]+)?$/\1/p' || true)`

func (m *Manager) CreateShims() ([]string, error) {
	if err := m.cfg.EnsureDir(m.cfg.ShimDir); err != nil {
		return nil, err
	}

	script := shimScript
	if goruntime.GOOS != "darwin" {
		script = strings.Replace(script, darwinComposerExtract, linuxComposerExtract, 1)
	}

	shimPath := filepath.Join(m.cfg.ShimDir, "igor-shim")
	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil {
		return nil, err
	}

	entries, _ := os.ReadDir(m.cfg.ShimDir)
	for _, e := range entries {
		full := filepath.Join(m.cfg.ShimDir, e.Name())
		if target, err := os.Readlink(full); err == nil && target == "igor-shim" {
			_ = os.Remove(full)
		}
	}

	tools := []string{"php", "composer", "frankenphp"}
	var created []string
	for _, t := range tools {
		if err := os.Symlink("igor-shim", filepath.Join(m.cfg.ShimDir, t)); err != nil {
			return nil, err
		}
		created = append(created, t)
	}
	for _, v := range m.runtimes.InstalledVersions() {
		short := strings.ReplaceAll(v, ".", "")
		for _, t := range tools {
			name := t + short
			if err := os.Symlink("igor-shim", filepath.Join(m.cfg.ShimDir, name)); err != nil {
				return nil, err
			}
			created = append(created, name)
		}
	}

	sort.Strings(created)
	return created, nil
}

func (m *Manager) IsOnPath() bool {
	path := os.Getenv("PATH")
	return strings.Contains(":"+path+":", ":"+m.cfg.ShimDir+":")
}
