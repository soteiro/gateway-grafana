// Package config carga y valida el catálogo de targets del gateway.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode define qué puede hacer el modelo sobre un target.
type Mode string

const (
	// ModeRead bloquea toda escritura. El upstream además arranca con --disable-write.
	ModeRead Mode = "read"
	// ModeConfirm permite escribir solo si la llamada trae confirm=true.
	ModeConfirm Mode = "confirm"
	// ModeWrite permite escribir libremente.
	ModeWrite Mode = "write"
)

type Config struct {
	Upstream Upstream `yaml:"upstream"`
	Tools    Tools    `yaml:"tools"`
	// AuditLog es la ruta de un archivo JSONL donde se registran las escrituras.
	// Vacío = se registran solo en stderr.
	AuditLog string   `yaml:"audit_log"`
	Targets  []Target `yaml:"targets"`
}

type Upstream struct {
	// Command y Args lanzan mcp-grafana por stdio. Por defecto: uvx mcp-grafana.
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}

type Tools struct {
	// Include limita las tools espejadas del upstream. Vacío = todas.
	Include []string `yaml:"include"`
	// Exclude quita tools del espejo (se aplica después de Include).
	Exclude []string `yaml:"exclude"`
}

type Target struct {
	ID          string   `yaml:"id"`
	Env         string   `yaml:"env"`
	Org         string   `yaml:"org"`
	Description string   `yaml:"description"`
	Aliases     []string `yaml:"aliases"`
	URL         string   `yaml:"url"`
	// OrgID se pasa como GRAFANA_ORG_ID. Opcional: los service account tokens ya
	// están atados a una org.
	OrgID    int    `yaml:"org_id"`
	TokenEnv string `yaml:"token_env"`
	Mode     Mode   `yaml:"mode"`
	// Args extra para el upstream de este target (se suman a upstream.args).
	Args []string `yaml:"args"`
}

// Token devuelve el token del target leído desde su variable de entorno.
func (t Target) Token() string { return os.Getenv(t.TokenEnv) }

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Load lee el YAML, aplica defaults y valida.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.applyDefaults()
	// Rutas relativas se resuelven respecto al YAML, no al cwd del cliente MCP.
	if c.AuditLog != "" && !filepath.IsAbs(c.AuditLog) {
		c.AuditLog = filepath.Join(filepath.Dir(path), c.AuditLog)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Upstream.Command == "" {
		c.Upstream.Command = "uvx"
		if len(c.Upstream.Args) == 0 {
			c.Upstream.Args = []string{"mcp-grafana"}
		}
	}
	for i := range c.Targets {
		if c.Targets[i].Mode == "" {
			c.Targets[i].Mode = ModeRead
		}
	}
}

func (c *Config) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("no hay targets definidos")
	}
	names := map[string]string{}
	for _, t := range c.Targets {
		if !idRe.MatchString(t.ID) {
			return fmt.Errorf("target %q: id inválido (usa minúsculas, números, - o _)", t.ID)
		}
		if t.URL == "" {
			return fmt.Errorf("target %q: falta url", t.ID)
		}
		if t.TokenEnv == "" {
			return fmt.Errorf("target %q: falta token_env", t.ID)
		}
		switch t.Mode {
		case ModeRead, ModeConfirm, ModeWrite:
		default:
			return fmt.Errorf("target %q: mode %q inválido (read|confirm|write)", t.ID, t.Mode)
		}
		for _, n := range append([]string{t.ID}, t.Aliases...) {
			k := strings.ToLower(n)
			if prev, dup := names[k]; dup {
				return fmt.Errorf("nombre %q repetido en targets %q y %q", n, prev, t.ID)
			}
			names[k] = t.ID
		}
	}
	return nil
}

// MissingTokens lista los targets cuya variable de token no está definida.
func (c *Config) MissingTokens() []string {
	var out []string
	for _, t := range c.Targets {
		if t.Token() == "" {
			out = append(out, fmt.Sprintf("%s (%s)", t.ID, t.TokenEnv))
		}
	}
	return out
}

// LoadCredentials carga los tokens desde YAML sin pisar variables del entorno.
func LoadCredentials(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var credentials struct {
		Tokens map[string]string `yaml:"tokens"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&credentials); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for key, value := range credentials.Tokens {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s: variable de token inválida", path)
		}
	}
	for key, value := range credentials.Tokens {
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("%s: no se pudo definir %s: %w", path, key, err)
			}
		}
	}
	return nil
}
