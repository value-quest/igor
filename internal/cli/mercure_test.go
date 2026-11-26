package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/value-quest/igor/internal/registry"
)

func writeMercureSite(t *testing.T, dotenv string) (*testEnv, string) {
	t.Helper()
	env := newTestEnv(t)
	env.setupCaddy()
	proj := env.makeProject("app")
	if dotenv != "" {
		_ = os.WriteFile(filepath.Join(proj, ".env"), []byte(dotenv), 0o644)
	}
	if _, err := env.app.Caddy.WriteSite(registry.Entry{Name: "app", Port: "8101", Type: "php", PHP: "8.5", Dir: proj, Docroot: "public"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(env.cfg.BackendCaddyfilePath("app"))
	if err != nil {
		t.Fatal(err)
	}
	return env, string(content)
}

func TestBackendAddsMercureHubForMercureSites(t *testing.T) {
	env, content := writeMercureSite(t, `APP_URL=https://app.test
# comment
BROADCAST_CONNECTION=mercure
MERCURE_URL="${APP_URL}/.well-known/mercure"
MERCURE_JWT_SECRET="s3cr\"et"
`)

	for _, want := range []string{
		"@compressible not path /.well-known/mercure*",
		"encode @compressible zstd br gzip",
		`issuer "https://app.test" {`,
		`jwt "s3cr\"et" HS256`,
		`resource_identifier "https://app.test/.well-known/mercure"`,
		"subscriptions",
		`path "` + filepath.Join(env.cfg.DataDir, "mercure", "app.db") + `"`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("expected %q in backend Caddyfile:\n%s", want, content)
		}
	}
	if strings.Index(content, "mercure {") > strings.Index(content, "php_server {") {
		t.Fatalf("mercure must come before php_server:\n%s", content)
	}
}

func TestBackendHasNoMercureHubWithoutMercureBroadcasting(t *testing.T) {
	for name, dotenv := range map[string]string{
		"no .env":          "",
		"other connection": "APP_URL=https://app.test\nBROADCAST_CONNECTION=reverb\nMERCURE_JWT_SECRET=secret\n",
		"missing secret":   "APP_URL=https://app.test\nBROADCAST_CONNECTION=mercure\n",
		"remote hub":       "APP_URL=https://app.test\nBROADCAST_CONNECTION=mercure\nMERCURE_JWT_SECRET=secret\nMERCURE_PUBLIC_URL=https://hub.example.com/.well-known/mercure\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, content := writeMercureSite(t, dotenv)
			if strings.Contains(content, "mercure") {
				t.Fatalf("expected no Mercure hub:\n%s", content)
			}
			if strings.Contains(content, "\"\n\n\tphp_server") {
				t.Fatalf("backend without Mercure must keep its previous layout:\n%s", content)
			}
		})
	}
}
