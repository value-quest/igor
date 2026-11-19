# igor

_Frankenstein's assistant: it serves your PHP projects through FrankenPHP so you don't have to._

Local PHP sites on macOS with the ValueQuest FrankenPHP suite: Laravel, Symfony, WordPress or plain PHP. It replaces Herd with a central Caddy proxy in front of one FrankenPHP backend per app.

```
Browser ──https://app.test──▶ front proxy (Caddy local CA, :80/:443)
                                 └──▶ http://127.0.0.1:81xx  FrankenPHP (per site, HTTPS=on)
App ────http://127.0.0.1:81xx──▶ other site directly (no TLS, like prod)
```

## Contents

- [PHP runtimes (ValueQuest FrankenPHP suite)](#php-runtimes-valuequest-frankenphp-suite)
  - [Install, update and uninstall](#install-update-and-uninstall)
  - [Requirements for the darwin build](#requirements-for-the-darwin-build)
- [Setup (once)](#setup-once)
  - [Trying igor next to Herd](#trying-igor-next-to-herd)
- [Daily use](#daily-use)
- [Workers (Reverb, queues, scheduler, ...)](#workers-reverb-queues-scheduler-)
- [Services (Redis, Mailpit, S3)](#services-redis-mailpit-s3)
- [PHP on the command line](#php-on-the-command-line)
- [Files](#files)
- [Notes](#notes)

## PHP runtimes (ValueQuest FrankenPHP suite)

igor uses the darwin builds from [value-quest/franken-php](https://github.com/value-quest/franken-php), installed side by side with `--versioned`:

```
~/frankenphp/php8.4/bin/{frankenphp,php,composer}
~/frankenphp/php8.5/bin/{frankenphp,php,composer}
```

All projects share these two runtimes. Each project picks one in its `.php-version`, so a PHP update is a single install, not one per project.

### Install, update and uninstall

```bash
igor install            # first time: installs every PHP version in the latest release
igor update             # later: installs new versions, then restarts everything on them
igor update --check     # installed vs. available PHP and FrankenPHP versions, changes nothing
igor update --force     # reinstall even if up to date, e.g. after a build fix
igor uninstall 8.4      # remove a PHP version the suite no longer ships
```

**`igor install`** is the same command as `igor update`, under the name that fits a new Mac. Run it before `igor setup`. It downloads `frankenphp-darwin-<arch>-php8x.tar.gz` for every PHP version in the latest release and runs each archive's `install.sh --versioned 8.x`. It checks that `php` and `frankenphp` run and records what it installed in `~/frankenphp/php8.x/.igor-release`.

**`igor update`** does the same, then runs `igor refresh --restart`, so the proxy, sites, workers and services restart on the new binaries. It installs only what changed. The suite rebuilds its archives every day into a release tagged with the FrankenPHP version, so igor compares versions instead of tags or dates. The versions come from the release's `versions.json` (`{"frankenphp": "1.13.5", "php84": "8.4.28", "php85": "8.5.13"}`). A daily rebuild with the same versions counts as up to date; a new PHP patch or FrankenPHP version gets installed. When the suite adds a minor version (e.g. 8.6), `update` installs it next to the others. Runtimes installed by hand show as `?` until their first `igor update`.

**`igor uninstall <8.x>`** removes `~/frankenphp/php8.x` and its backups after you confirm (`--yes` skips the question). It refuses while a site still uses that version, so switch those projects first: change their `.php-version` and run `igor refresh`. It also refuses to remove the last runtime, which the front proxy needs. Afterwards it runs `igor refresh`, so the proxy and the `php8x` shims move to the remaining versions. `igor update --check` lists installed versions the suite no longer ships.

The installer keeps the previous version as `~/frankenphp/php8.x.backup.<date>`, and igor keeps only the newest backup. To roll back, move the backup back to `php8.x` and run `igor refresh --restart`.

<details><summary>Manual install (what <code>igor install</code> does)</summary>

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
```

</details>

### Requirements for the darwin build

Each binary must run straight from its folder. On macOS that means dylibs in `../lib`, found through an `@executable_path/../lib` rpath, instead of the `LD_LIBRARY_PATH` wrappers used on Linux. igor never calls a binary from a project folder.

## Setup (once)

```bash
cp igor ~/bin/ && chmod +x ~/bin/igor     # or anywhere on PATH
igor install                              # PHP runtimes, see above
# quit Herd first (it holds ports 80/443)
igor setup
echo 'eval "$(direnv hook zsh)"' >> ~/.zshrc
```

`setup` installs dnsmasq and direnv through Homebrew. It points `*.test` to 127.0.0.1, starts the front proxy as a LaunchAgent, and puts the Caddy local CA into the macOS keychain. It asks for sudo for dnsmasq (port 53) and for `/etc/resolver/test`.

### Trying igor next to Herd

igor and Herd both need ports 80/443, so only one of them can run at a time:

```bash
igor stop     # proxy, sites, workers, services; they stay stopped after a restart of the Mac
              # -> start Herd
igor start    # quit Herd first; igor refuses to start while 80/443 are taken
```

Your sites, configs and service data stay as they are in between. dnsmasq keeps running; it sends `*.test` to 127.0.0.1 for igor and Herd alike.

## Daily use

```bash
cd ~/Code/portal && igor add portal       # https://portal.test, newest PHP
cd ~/Code/sso    && igor add sso --php 8.4
cd ~/Code/blog   && igor add blog --docroot .   # WordPress: serve the project folder itself
igor proxy reverb 8080                    # route to a service you run yourself
igor service add redis -- redis-server    # background service, see below
igor list
igor stop portal / start portal / logs portal
igor stop / igor start                    # everything, e.g. to switch to Herd and back
igor workers                              # sync Procfile.dev workers
igor refresh                              # regenerate everything, restart what changed
igor info sso                             # internal URL + .env hints
```

`add` serves `public/` if the project has one (Laravel, Symfony, Slim, ...) and the project folder itself otherwise. For anything else, pass `--docroot` with a folder inside the project, e.g. `--docroot web`. Requests for files that don't exist go to the document root's `index.php`, which is what front controllers and WordPress permalinks need. To change the document root, run `igor rm <name>` and add the site again.

`add` writes `.php-version` and an `.envrc`. Inside the project, `php`, `composer` and `frankenphp` then come from the shared runtime, and that includes Claude Code when you start it there. To switch a project's PHP version, edit `.php-version`, then run `igor refresh`.

## Workers (Reverb, queues, scheduler, ...)

Long-running commands go in a `Procfile.dev` in the project root, one `name: command` per line:

```
reverb:   php artisan reverb:start
queue:    php artisan queue:listen --tries=3
schedule: php artisan schedule:work
```

`igor add` starts them along with the site. After you edit the file, run `igor workers` in the project (or `igor workers <name>` from anywhere). That starts new lines, restarts changed ones, and removes deleted ones.

Each worker is a LaunchAgent, like the site itself. It starts at login and restarts when it exits, at most once every 10 seconds. Commands run in the project folder through bash, with the site's PHP first on PATH, followed by `vendor/bin` and `node_modules/.bin`.

```bash
igor list                       # workers appear under their site
igor logs portal:reverb         # ~/.igor/logs/portal@reverb.log
igor restart portal:queue       # one worker
igor restart portal             # the site and all its workers
igor stop portal                # same for stop/start
```

Locally, prefer `queue:listen` over `queue:work`: it picks up code changes without a restart. Only list commands that keep running. A command that exits is started again every 10 seconds.

## Services (Redis, Mailpit, S3)

Shared services run natively as LaunchAgents too. You don't need Docker or a VM. Install the binary, then hand igor the command:

```bash
brew install redis mailpit

igor service add redis -- redis-server --port 6379
igor service add mail --route 8025 -- mailpit --smtp 127.0.0.1:2525 --listen 127.0.0.1:8025 --database mailpit.db
igor service add s3 --route 9001 --env MINIO_ROOT_USER=admin --env MINIO_ROOT_PASSWORD=change-me \
    -- silo server . --address 127.0.0.1:9000 --console-address 127.0.0.1:9001
```

- Each service runs in `~/.igor/data/<name>`, so relative paths in the command (Redis' `dump.rdb`, `mailpit.db`, `.` for Silo) keep their data there.
- `--route PORT` serves the service's web UI as `https://<name>.test`, e.g. `https://mail.test` for Mailpit.
- `--env KEY=VALUE` sets environment variables. You can repeat it.
- Commands without a path are looked up in Homebrew first. Silo has no formula: download the macOS binary from its GitHub releases and pass its full path. Check its arguments by hand once first; the line above assumes it works like MinIO.
- To change a command, run `igor service add` again with the same name. igor restarts the service only if something changed.
- `igor start|stop|restart|logs|info <name>` work as for sites. `igor rm <name>` removes the service and keeps its data.

Apps connect directly, without the proxy:

```dotenv
REDIS_HOST=127.0.0.1
MAIL_HOST=127.0.0.1
MAIL_PORT=2525
AWS_ENDPOINT=http://127.0.0.1:9000
AWS_USE_PATH_STYLE_ENDPOINT=true
```

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
| `~/.igor/backends/*.Caddyfile` | generated FrankenPHP config per site (sets `HTTPS=on` for PHP) |
| `~/.igor/php/99-igor.ini` | shared php.ini settings (web and CLI, via `PHP_INI_SCAN_DIR`) |
| `~/.igor/data/<service>/` | working folder and data of each service |
| `~/.igor/logs/` | one log per site and per worker (`<site>@<worker>.log`), plus `proxy.log` |
| `~/Library/LaunchAgents/dev.igor.*.plist` | LaunchAgents for the proxy, sites and workers (`<site>@<worker>`): start at login, restart if they crash |

## Notes

- Everything igor runs starts at login. `igor stop <name>` stops a site, worker or service and keeps it stopped after a restart of the Mac, until `igor start <name>`. An idle FrankenPHP server uses a few tens of MB.
- PHP sees every request as HTTPS (`HTTPS=on`), so Laravel, Symfony and WordPress generate `https://` URLs and assets behind the front proxy without trusting any proxy. That includes direct calls to `http://127.0.0.1:<port>`. The client IP is still `127.0.0.1`.
- Logs are not rotated. `igor logs --clear <name>` empties one (`portal:queue` for a worker), `igor logs --clear` all of them. Running services keep logging into the emptied file.
- The front proxy listens on all interfaces, so sites are reachable from your LAN unless the macOS firewall blocks them. Binding only to 127.0.0.1 on ports 80/443 needs root on macOS.
- The script targets the bash 3.2 that ships with macOS and needs nothing else.
