// Package upstream administra las sesiones MCP hacia los procesos mcp-grafana,
// uno por target, levantados bajo demanda.
package upstream

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cursor-cl/gateway-grafana/internal/config"
)

// Dialer abre una sesión MCP hacia el upstream de un target.
type Dialer func(ctx context.Context, t config.Target) (*mcp.ClientSession, error)

// Pool mantiene una sesión viva por target y la recrea si el proceso muere.
type Pool struct {
	dial    Dialer
	targets map[string]config.Target
	mu      sync.Mutex
	conns   map[string]*conn
}

type conn struct {
	mu      sync.Mutex
	session *mcp.ClientSession
}

func NewPool(targets []config.Target, dial Dialer) *Pool {
	p := &Pool{dial: dial, targets: map[string]config.Target{}, conns: map[string]*conn{}}
	for _, t := range targets {
		p.targets[t.ID] = t
		p.conns[t.ID] = &conn{}
	}
	return p
}

// Session devuelve la sesión del target, levantando el upstream si hace falta.
func (p *Pool) Session(ctx context.Context, id string) (*mcp.ClientSession, error) {
	p.mu.Lock()
	c, ok := p.conns[id]
	t := p.targets[id]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("target desconocido %q", id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		return c.session, nil
	}
	s, err := p.dial(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("no se pudo iniciar el upstream de %q: %w", id, err)
	}
	c.session = s
	go func() {
		err := s.Wait()
		slog.Warn("upstream terminó", "target", id, "err", err)
		c.mu.Lock()
		if c.session == s {
			c.session = nil
		}
		c.mu.Unlock()
	}()
	return s, nil
}

// CallTool invoca una tool en el upstream del target.
func (p *Pool) CallTool(ctx context.Context, id, name string, args map[string]any) (*mcp.CallToolResult, error) {
	s, err := p.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
}

// Close cierra todas las sesiones abiertas.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.mu.Lock()
		if c.session != nil {
			c.session.Close()
			c.session = nil
		}
		c.mu.Unlock()
	}
}

// CommandDialer lanza mcp-grafana como subproceso stdio con las credenciales
// del target. Los targets en modo read arrancan con --disable-write.
func CommandDialer(up config.Upstream, version string) Dialer {
	client := mcp.NewClient(&mcp.Implementation{Name: "gateway-grafana", Version: version}, nil)
	return func(ctx context.Context, t config.Target) (*mcp.ClientSession, error) {
		token := t.Token()
		if token == "" {
			return nil, fmt.Errorf("la variable %s no está definida", t.TokenEnv)
		}
		args := append([]string{}, up.Args...)
		args = append(args, t.Args...)
		if t.Mode == config.ModeRead {
			args = append(args, "--disable-write")
		}
		// No se usa CommandContext: el proceso vive más que el request que lo creó.
		cmd := exec.Command(up.Command, args...)
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(),
			"GRAFANA_URL="+t.URL,
			"GRAFANA_SERVICE_ACCOUNT_TOKEN="+token,
		)
		if t.OrgID > 0 {
			cmd.Env = append(cmd.Env, "GRAFANA_ORG_ID="+strconv.Itoa(t.OrgID))
		}
		for k, v := range up.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		slog.Info("iniciando upstream", "target", t.ID, "mode", t.Mode)
		return client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	}
}
