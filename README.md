# igor

_Frankenstein's assistant: it serves your Laravel projects through FrankenPHP so you don't have to._

Local Laravel sites on macOS with the ValueQuest FrankenPHP suite. It replaces Herd and mirrors prod: a central Caddy proxy in front of one FrankenPHP backend per app.

```
Browser ──https://app.test──▶ front proxy (Caddy local CA, :80/:443)
                                 └──▶ http://127.0.0.1:81xx  FrankenPHP php-server (per site)
App ────http://127.0.0.1:81xx──▶ other site directly (no TLS, like prod)
```

## PHP runtimes (ValueQuest FrankenPHP suite)

igor uses the darwin builds from [value-quest/franken-php](https://github.com/value-quest/franken-php), installed side by side with `--versioned`:

```
~/frankenphp/php8.4/bin/{frankenphp,php,composer}
~/frankenphp/php8.5/bin/{frankenphp,php,composer}
```

All projects share these two runtimes. Each project picks one in its `.php-version`, so a PHP update is a single install, not one per project.

### Install

Run it as your normal user, **without sudo**. Always use `--versioned` on macOS: the normal install writes to `~/bin` and `~/.bash_profile` and looks for running servers through `/proc`, which macOS doesn't have.

```bash
cd /tmp
gh release download -R value-quest/franken-php -p 'frankenphp-darwin-arm64-*' --clobber

for v in 8.4 8.5; do
  short=php${v/./}                       # php84, php85
  rm -rf "/tmp/fp-$short" && mkdir "/tmp/fp-$short"
  tar -xzf "frankenphp-darwin-arm64-$short.tar.gz" -C "/tmp/fp-$short"
  "/tmp/fp-$short/install.sh" --versioned "$v"
done

~/frankenphp/php8.5/bin/php -v
~/frankenphp/php8.5/bin/frankenphp version
```

The tarball names follow the Linux pattern (`frankenphp-linux-arm64-php84.tar.gz`). Change the pattern if the darwin release files are named differently.

### Update

Run the same commands again. The installer replaces `~/frankenphp/php8.x` and keeps the previous version as `~/frankenphp/php8.x.backup.<date>`. Delete those backups once the new version works. Then restart whatever runs on the old binaries:

```bash
for s in $(cut -f1,3 ~/.igor/sites.tsv | awk '$2 == "php" { print $1 }'); do igor restart "$s"; done
launchctl kickstart -k gui/$(id -u)/dev.igor.proxy          # front proxy
```

When the suite moves on to a new minor version (e.g. drops 8.4 and adds 8.6), change `.php-version` in the projects that still use the dropped one and re-add them (`igor rm <name> && igor add <name>`). The front proxy uses the newest runtime installed when you ran `igor setup`, so run `igor setup` again after removing that runtime.

### Requirements for the darwin build

Each binary must run straight from its folder. On macOS that means dylibs in `../lib`, found through an `@executable_path/../lib` rpath, instead of the `LD_LIBRARY_PATH` wrappers used on Linux. igor never calls a binary from a project folder.

## Setup (once)

```bash
cp igor ~/bin/ && chmod +x ~/bin/igor     # or anywhere on PATH
# quit Herd first (it holds ports 80/443)
igor setup
echo 'eval "$(direnv hook zsh)"' >> ~/.zshrc
```

`setup` installs dnsmasq and direnv through Homebrew. It points `*.test` to 127.0.0.1, starts the front proxy as a LaunchAgent, and puts the Caddy local CA into the macOS keychain. It asks for sudo for dnsmasq (port 53) and for `/etc/resolver/test`.

## Daily use

```bash
cd ~/Code/portal && igor add              # https://portal.test, newest PHP
cd ~/Code/sso    && igor add sso --php 8.4
igor proxy reverb 8080                    # route to a service you run yourself
igor proxy mail 8025                      # Mailpit UI
igor list
igor stop portal / start portal / logs portal
igor info sso                             # internal URL + .env hints
```

`add` writes `.php-version` and an `.envrc`. Inside the project, `php`, `composer` and `frankenphp` then come from the shared runtime, and that includes Claude Code when you start it there. To switch a project's PHP version, edit `.php-version`, then run `igor rm <name> && igor add <name>`.

## PHP on the command line

`igor setup` (or `igor shims` on its own) puts shims in `~/.igor/bin`:

| Command | PHP version |
|---|---|
| `php`, `composer`, `frankenphp` | nearest `.php-version` from the current folder upwards, else the newest installed |
| `php84`, `composer84`, `frankenphp84`, `php85`, ... | that version, always |

They work in any folder: no direnv and no `igor add` needed. Put `~/.igor/bin` first on PATH, at the end of `~/.zshrc` so it comes after Herd's lines:

```sh
export PATH="$HOME/.igor/bin:$PATH"
```

Run `igor shims` again after you install or remove a runtime, so the numbered commands match.

## Files

| Path | What |
|---|---|
| `~/.igor/sites.tsv` | registry: name, port, type, PHP version, folder |
| `~/.igor/Caddyfile`, `~/.igor/sites/*.caddy` | front proxy config, one route per site |
| `~/.igor/php/99-igor.ini` | shared php.ini settings (web and CLI, via `PHP_INI_SCAN_DIR`) |
| `~/.igor/logs/` | one log per site, plus `proxy.log` |
| `~/Library/LaunchAgents/dev.igor.*.plist` | LaunchAgents: start at login, restart if they crash |

## Notes

- Sites start at login. Stop the ones you don't need with `igor stop`; an idle php-server uses a few tens of MB.
- Queue workers, the scheduler and Vite are not managed here. Run them per project, e.g. with `composer dev`.
- The front proxy listens on all interfaces, so sites are reachable from your LAN unless the macOS firewall blocks them. Binding only to 127.0.0.1 on ports 80/443 needs root on macOS.
- The script targets the bash 3.2 that ships with macOS and needs nothing else.
