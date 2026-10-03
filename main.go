// gateway-grafana: un único servidor MCP que enruta hacia varias instancias y
// organizaciones de Grafana (vía mcp-grafana) usando un parámetro `target`.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cursor-cl/gateway-grafana/internal/config"
	"github.com/cursor-cl/gateway-grafana/internal/gateway"
	"github.com/cursor-cl/gateway-grafana/internal/upstream"
)

const version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "targets.yaml", "ruta al catálogo de targets")
	envFile := flag.String("env-file", "", "archivo KEY=VALUE con los tokens (opcional)")
	check := flag.Bool("check", false, "verifica cada target (user_info) y termina")
	flag.Parse()

	// stdout es el canal MCP: todo el logging va a stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := run(*cfgPath, *envFile, *check); err != nil {
		fmt.Fprintln(os.Stderr, "gateway-grafana:", err)
		os.Exit(1)
	}
}

func run(cfgPath, envFile string, check bool) error {
	if envFile != "" {
		if err := config.LoadEnvFile(envFile); err != nil {
			return err
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if missing := cfg.MissingTokens(); len(missing) > 0 {
		slog.Warn("targets sin token (fallarán al usarse)", "targets", missing)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool := upstream.NewPool(cfg.Targets, upstream.CommandDialer(cfg.Upstream, version))
	defer pool.Close()

	if check {
		return runCheck(ctx, cfg, pool)
	}

	var audit io.Writer
	if cfg.AuditLog != "" {
		f, err := os.OpenFile(cfg.AuditLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		audit = f
	}

	server, err := gateway.New(cfg, pool, audit, version).Server(ctx)
	if err != nil {
		return err
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

func runCheck(ctx context.Context, cfg *config.Config, pool *upstream.Pool) error {
	failed := 0
	for _, t := range cfg.Targets {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res, err := pool.CallTool(cctx, t.ID, "user_info", nil)
		cancel()
		status := "OK"
		switch {
		case err != nil:
			status = "ERROR: " + err.Error()
		case res.IsError:
			status = "ERROR: " + firstText(res)
		}
		if status != "OK" {
			failed++
		}
		fmt.Printf("%-24s %-8s %-8s %s\n", t.ID, t.Env, t.Mode, status)
	}
	if failed > 0 {
		return fmt.Errorf("%d target(s) con error", failed)
	}
	return nil
}

func firstText(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return "(sin detalle)"
}
