package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "targets.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(write(t, `
targets:
  - id: prod-a
    env: prod
    url: https://g
    token_env: TOK_A
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Targets[0].Mode != ModeRead {
		t.Errorf("mode por defecto = %q, esperado read", c.Targets[0].Mode)
	}
	if c.Upstream.Command != "uvx" || c.Upstream.Args[0] != "mcp-grafana" {
		t.Errorf("upstream por defecto = %+v", c.Upstream)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"alias repetido": `
targets:
  - {id: a, url: u, token_env: T, aliases: [x]}
  - {id: b, url: u, token_env: T, aliases: [X]}`,
		"mode inválido": `
targets:
  - {id: a, url: u, token_env: T, mode: admin}`,
		"campo desconocido": `
targets:
  - {id: a, url: u, token_env: T, token: secreto}`,
		"sin token_env": `
targets:
  - {id: a, url: u}`,
	}
	for name, yml := range cases {
		if _, err := Load(write(t, yml)); err == nil {
			t.Errorf("%s: se esperaba error", name)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comentario\nGW_TEST_A=\"uno\"\nexport GW_TEST_B=dos\n"), 0o600)
	t.Setenv("GW_TEST_B", "existente")
	t.Setenv("GW_TEST_A", "")
	os.Unsetenv("GW_TEST_A")
	if err := LoadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("GW_TEST_A"); got != "uno" {
		t.Errorf("GW_TEST_A = %q", got)
	}
	if got := os.Getenv("GW_TEST_B"); !strings.EqualFold(got, "existente") {
		t.Errorf("GW_TEST_B no debe pisarse, got %q", got)
	}
}
