// Package gateway expone un único servidor MCP que enruta las tools de
// mcp-grafana hacia el target (entorno + organización) que elija el modelo.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cursor-cl/gateway-grafana/internal/config"
	"github.com/cursor-cl/gateway-grafana/internal/upstream"
)

// Nombres de los parámetros que el gateway agrega a cada tool espejada.
const (
	targetParam  = "target"
	confirmParam = "confirm"
)

type Gateway struct {
	cfg     *config.Config
	pool    *upstream.Pool
	byName  map[string]config.Target // id y aliases en minúsculas
	audit   *auditLog
	version string
	// confirmable indica si existe algún target en modo confirm.
	confirmable bool
}

func New(cfg *config.Config, pool *upstream.Pool, audit io.Writer, version string) *Gateway {
	g := &Gateway{
		cfg:     cfg,
		pool:    pool,
		byName:  map[string]config.Target{},
		audit:   &auditLog{w: audit},
		version: version,
	}
	for _, t := range cfg.Targets {
		g.byName[strings.ToLower(t.ID)] = t
		for _, a := range t.Aliases {
			g.byName[strings.ToLower(a)] = t
		}
		if t.Mode == config.ModeConfirm {
			g.confirmable = true
		}
	}
	return g
}

// Server arma el servidor MCP. Lee la lista de tools desde un upstream
// (preferentemente uno con escritura habilitada, para tener el catálogo completo).
func (g *Gateway) Server(ctx context.Context) (*mcp.Server, error) {
	src := g.schemaSource()
	sess, err := g.pool.Session(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	var upstreamTools []*mcp.Tool
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("listando tools de %q: %w", src.ID, err)
		}
		if g.included(t.Name) {
			upstreamTools = append(upstreamTools, t)
		}
	}

	s := mcp.NewServer(
		&mcp.Implementation{Name: "gateway-grafana", Version: g.version},
		&mcp.ServerOptions{Instructions: g.instructions()},
	)
	s.AddTool(listTargetsTool, g.listTargets)
	hasSearch := false
	for _, t := range upstreamTools {
		mirrored, withConfirm, err := g.mirror(t)
		if err != nil {
			return nil, err
		}
		s.AddTool(mirrored, g.proxy(t, withConfirm))
		hasSearch = hasSearch || t.Name == "search_dashboards"
	}
	if hasSearch {
		s.AddTool(searchEverywhereTool(g.envs()), g.searchEverywhere)
	}
	slog.Info("gateway listo", "tools", len(upstreamTools), "targets", len(g.cfg.Targets), "schema_source", src.ID)
	return s, nil
}

func (g *Gateway) schemaSource() config.Target {
	for _, t := range g.cfg.Targets {
		if t.Mode != config.ModeRead {
			return t
		}
	}
	return g.cfg.Targets[0]
}

func (g *Gateway) included(name string) bool {
	inc := g.cfg.Tools.Include
	if len(inc) > 0 && !slices.Contains(inc, name) {
		return false
	}
	return !slices.Contains(g.cfg.Tools.Exclude, name)
}

func (g *Gateway) envs() []string {
	var out []string
	for _, t := range g.cfg.Targets {
		if t.Env != "" && !slices.Contains(out, t.Env) {
			out = append(out, t.Env)
		}
	}
	return out
}

// resolve acepta el id o un alias, sin distinguir mayúsculas.
func (g *Gateway) resolve(name string) (config.Target, error) {
	if t, ok := g.byName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return t, nil
	}
	ids := make([]string, len(g.cfg.Targets))
	for i, t := range g.cfg.Targets {
		ids[i] = t.ID
	}
	if name == "" {
		return config.Target{}, fmt.Errorf("falta el parámetro %q; valores válidos: %s (usa list_targets para ver qué contiene cada uno)", targetParam, strings.Join(ids, ", "))
	}
	return config.Target{}, fmt.Errorf("target %q desconocido; valores válidos: %s (usa list_targets)", name, strings.Join(ids, ", "))
}

// mirror copia la definición de la tool upstream agregando target (y confirm si aplica).
func (g *Gateway) mirror(t *mcp.Tool) (*mcp.Tool, bool, error) {
	write := isWriteTool(t)
	withConfirm := write && g.confirmable
	schema, err := injectParams(t.InputSchema, g.cfg.Targets, withConfirm)
	if err != nil {
		return nil, false, fmt.Errorf("tool %q: %w", t.Name, err)
	}
	desc := t.Description
	if write {
		note := "ESCRITURA: bloqueada en targets de solo lectura"
		if withConfirm {
			note += "; en targets con confirmación requiere confirm=true tras aprobación del usuario"
		}
		desc = "[" + note + "] " + desc
	}
	return &mcp.Tool{
		Name:        t.Name,
		Title:       t.Title,
		Description: desc,
		Annotations: t.Annotations,
		InputSchema: schema,
		// OutputSchema se omite: los errores del gateway no traen structuredContent.
	}, withConfirm, nil
}

func injectParams(in any, targets []config.Target, withConfirm bool) (map[string]any, error) {
	schema := map[string]any{}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	if schema["type"] == nil {
		schema["type"] = "object"
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	if _, clash := props[targetParam]; clash {
		return nil, fmt.Errorf("el upstream ya define el parámetro %q", targetParam)
	}
	ids := make([]any, len(targets))
	var lines []string
	for i, t := range targets {
		ids[i] = t.ID
		lines = append(lines, fmt.Sprintf("%s = %s/%s [%s]", t.ID, t.Env, t.Org, t.Mode))
	}
	props[targetParam] = map[string]any{
		"type":        "string",
		"enum":        ids,
		"description": "Instancia y organización de Grafana sobre la que actuar. " + strings.Join(lines, "; ") + ". Si no está claro cuál usar, llama primero a list_targets.",
	}
	if withConfirm {
		if _, clash := props[confirmParam]; clash {
			return nil, fmt.Errorf("el upstream ya define el parámetro %q", confirmParam)
		}
		props[confirmParam] = map[string]any{
			"type":        "boolean",
			"description": "Solo para targets con mode=confirm: true únicamente después de que el usuario aprobó explícitamente este cambio.",
		}
	}
	schema["properties"] = props
	req, _ := schema["required"].([]any)
	schema["required"] = append([]any{targetParam}, req...)
	return schema, nil
}

func (g *Gateway) proxy(tool *mcp.Tool, withConfirm bool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errorResult("argumentos inválidos: %v", err), nil
			}
		}
		name, _ := args[targetParam].(string)
		delete(args, targetParam)
		confirmed := false
		if withConfirm {
			confirmed, _ = args[confirmParam].(bool)
			delete(args, confirmParam)
		}
		t, err := g.resolve(name)
		if err != nil {
			return errorResult("%v", err), nil
		}
		write := isWriteCall(tool, args)
		if write {
			if err := checkWrite(t, tool.Name, confirmed); err != nil {
				g.audit.record(t, tool.Name, args, "denied", err)
				return errorResult("%v", err), nil
			}
		}
		res, err := g.pool.CallTool(ctx, t.ID, tool.Name, args)
		if write {
			g.audit.record(t, tool.Name, args, "allowed", err)
		}
		if err != nil {
			return errorResult("[%s] error llamando al upstream: %v", t.ID, err), nil
		}
		return res, nil
	}
}

var listTargetsTool = &mcp.Tool{
	Name: "list_targets",
	Description: "Lista los Grafana (entorno + organización) disponibles en este gateway, con su descripción, alias y permisos. " +
		"Úsala para decidir qué valor pasar en el parámetro `target` del resto de las tools.",
	Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"env": map[string]any{"type": "string", "description": "Filtra por entorno (por ejemplo prod o testing)."},
		},
	},
}

type targetInfo struct {
	ID          string   `json:"id"`
	Env         string   `json:"env"`
	Org         string   `json:"org"`
	Description string   `json:"description,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	Mode        string   `json:"mode"`
	URL         string   `json:"url"`
}

func (g *Gateway) listTargets(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Env string `json:"env"`
	}
	if len(req.Params.Arguments) > 0 {
		_ = json.Unmarshal(req.Params.Arguments, &in)
	}
	var out []targetInfo
	for _, t := range g.cfg.Targets {
		if in.Env != "" && !strings.EqualFold(in.Env, t.Env) {
			continue
		}
		out = append(out, targetInfo{t.ID, t.Env, t.Org, t.Description, t.Aliases, string(t.Mode), t.URL})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

func searchEverywhereTool(envs []string) *mcp.Tool {
	env := map[string]any{"type": "string", "description": "Limita la búsqueda a un entorno."}
	if len(envs) > 0 {
		e := make([]any, len(envs))
		for i, v := range envs {
			e[i] = v
		}
		env["enum"] = e
	}
	return &mcp.Tool{
		Name: "search_everywhere",
		Description: "Busca dashboards por texto en todos los targets a la vez (o en un entorno) y agrupa los resultados por target. " +
			"Úsala cuando el usuario no dice en qué organización está algo.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Texto a buscar en títulos de dashboards."},
				"env":   env,
			},
			"required": []any{"query"},
		},
	}
}

func (g *Gateway) searchEverywhere(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Query string `json:"query"`
		Env   string `json:"env"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
		return errorResult("argumentos inválidos: %v", err), nil
	}
	var targets []config.Target
	for _, t := range g.cfg.Targets {
		if in.Env == "" || strings.EqualFold(in.Env, t.Env) {
			targets = append(targets, t)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	sections := make([]string, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() {
			res, err := g.pool.CallTool(ctx, t.ID, "search_dashboards", map[string]any{"query": in.Query})
			var body string
			switch {
			case err != nil:
				body = "ERROR: " + err.Error()
			case res.IsError:
				body = "ERROR: " + resultText(res)
			default:
				body = resultText(res)
			}
			sections[i] = fmt.Sprintf("## target=%s (%s/%s)\n%s", t.ID, t.Env, t.Org, body)
		})
	}
	wg.Wait()
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(sections, "\n\n")}}}, nil
}

func (g *Gateway) instructions() string {
	var b strings.Builder
	b.WriteString("Gateway único hacia varias instancias y organizaciones de Grafana.\n")
	b.WriteString("Todas las tools de Grafana exigen el parámetro `target`, que identifica entorno + organización. ")
	b.WriteString("Si el usuario no indica la organización, usa list_targets o search_everywhere antes de adivinar. ")
	b.WriteString("Indica siempre en tu respuesta de qué target salen los datos.\n")
	b.WriteString("Permisos: read = solo lectura; confirm = escrituras solo con aprobación explícita del usuario y confirm=true; write = libre. ")
	b.WriteString("Para probar cambios usa targets de testing antes que prod.\n\nTargets:\n")
	for _, t := range g.cfg.Targets {
		fmt.Fprintf(&b, "- %s: env=%s org=%s mode=%s", t.ID, t.Env, t.Org, t.Mode)
		if len(t.Aliases) > 0 {
			fmt.Fprintf(&b, " alias=%s", strings.Join(t.Aliases, ","))
		}
		if t.Description != "" {
			fmt.Fprintf(&b, " — %s", strings.TrimSpace(t.Description))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func errorResult(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

func resultText(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// auditLog registra en JSONL cada intento de escritura.
type auditLog struct {
	mu sync.Mutex
	w  io.Writer
}

func (a *auditLog) record(t config.Target, tool string, args map[string]any, decision string, callErr error) {
	argsJSON, _ := json.Marshal(args)
	if len(argsJSON) > 4096 {
		argsJSON = append(argsJSON[:4096], []byte("…")...)
	}
	entry := map[string]any{
		"time":     time.Now().Format(time.RFC3339),
		"target":   t.ID,
		"env":      t.Env,
		"mode":     t.Mode,
		"tool":     tool,
		"decision": decision,
		"args":     string(argsJSON),
	}
	if callErr != nil {
		entry["error"] = callErr.Error()
	}
	slog.Info("escritura", "target", t.ID, "tool", tool, "decision", decision)
	if a.w == nil {
		return
	}
	line, _ := json.Marshal(entry)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.w.Write(append(line, '\n'))
}
