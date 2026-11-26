package caddy

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// mercureDirectives returns the Caddyfile directives for FrankenPHP's built-in
// Mercure hub when the site's .env broadcasts through Mercure, mirroring the
// hub Laravel Octane generates from the "mercure" broadcasting connection.
// It returns "" when the site doesn't use Mercure or uses a hub on another origin.
// Each hub needs its own Bolt database, since hubs can't share one.
func mercureDirectives(projectDir, boltPath string) string {
	env := readDotenv(filepath.Join(projectDir, ".env"))

	secret := env["MERCURE_JWT_SECRET"]
	appOrigin := origin(env["APP_URL"])
	if env["BROADCAST_CONNECTION"] != "mercure" || secret == "" || appOrigin == "" {
		return ""
	}

	hubURL := env["MERCURE_PUBLIC_URL"]
	if hubURL == "" {
		hubURL = env["MERCURE_URL"]
	}
	if hubURL != "" && origin(hubURL) != appOrigin {
		return ""
	}
	if hubURL == "" {
		hubURL = appOrigin + "/.well-known/mercure"
	}

	issuer := env["MERCURE_JWT_ISSUER"]
	if issuer == "" {
		issuer = env["APP_URL"]
	}

	return fmt.Sprintf(`

	# Compressing the Mercure stream buffers events and costs memory per subscriber.
	@compressible not path /.well-known/mercure*
	encode @compressible zstd br gzip

	mercure {
		issuer %s {
			publisher {
				jwt %s HS256
			}
			subscriber {
				jwt %s HS256
			}
		}
		resource_identifier %s
		subscriptions
		transport bolt {
			path %s
		}
	}
`, quote(issuer), quote(secret), quote(secret), quote(hubURL), quote(boltPath))
}

var dotenvReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// readDotenv parses KEY=VALUE lines, strips surrounding quotes and expands
// ${VAR} references to earlier keys, like Laravel's dotenv loader.
func readDotenv(path string) map[string]string {
	values := map[string]string{}

	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		quoted := len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0]
		switch {
		case quoted && value[0] == '\'':
			values[key] = value[1 : len(value)-1]
			continue
		case quoted:
			value = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(value[1 : len(value)-1])
		default:
			if index := strings.Index(value, " #"); index >= 0 {
				value = strings.TrimSpace(value[:index])
			}
		}

		values[key] = dotenvReference.ReplaceAllStringFunc(value, func(reference string) string {
			return values[dotenvReference.FindStringSubmatch(reference)[1]]
		})
	}

	return values
}

// origin reduces a URL to its scheme, host and port, or "" when it isn't absolute.
func origin(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// quote renders a value as a double-quoted Caddyfile token.
func quote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}
