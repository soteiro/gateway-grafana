package gateway

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cursor-cl/gateway-grafana/internal/config"
	"github.com/cursor-cl/gateway-grafana/internal/upstream"
)

var testTargets = []config.Target{
	{ID: "testing-profesor", Env: "testing", Org: "profesor", Mode: config.ModeWrite, Aliases: []string{"profe-test"}},
	{ID: "prod-vitacura", Env: "prod", Org: "vitacura", Mode: config.ModeRead, Aliases: []string{"vitacura"}},
	{ID: "prod-profesor", Env: "prod", Org: "profesor", Mode: config.ModeConfirm},
}

// recorder guarda las llamadas que llegan a los upstreams falsos.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.calls = append(r.calls, s); r.mu.Unlock() }

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// fakeDialer levanta un servidor MCP en memoria por target que responde
// "<target>:<tool>:<args>" para poder verificar el enrutamiento.
func fakeDialer(calls *recorder) upstream.Dialer {
	return func(ctx context.Context, t config.Target) (*mcp.ClientSession, error) {
		s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
		add := func(name string, ro bool) {
			tool := &mcp.Tool{
				Name:        name,
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: ro},
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"query": map[string]any{"type": "string"}, "method": map[string]any{"type": "string"}},
					"required":   []any{"query"},
				},
			}
			s.AddTool(tool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				msg := t.ID + ":" + name + ":" + string(req.Params.Arguments)
				calls.add(msg)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil
			})
		}
		add("search_dashboards", true)
		add("update_dashboard", false)
		add("grafana_api_request", false)
		add("user_info", true)
		ct, st := mcp.NewInMemoryTransports()
		if _, err := s.Connect(ctx, st, nil); err != nil {
			return nil, err
		}
		return mcp.NewClient(&mcp.Implementation{Name: "gw", Version: "0"}, nil).Connect(ctx, ct, nil)
	}
}

func setup(t *testing.T, cfg *config.Config) (*mcp.ClientSession, *recorder, *strings.Builder) {
	t.Helper()
	ctx := context.Background()
	calls := &recorder{}
	pool := upstream.NewPool(cfg.Targets, fakeDialer(calls))
	t.Cleanup(pool.Close)
	audit := &strings.Builder{}
	srv, err := New(cfg, pool, audit, "test").Server(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, calls, audit
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return resultText(res), res.IsError
}

func TestToolsAreMirroredWithTarget(t *testing.T) {
	cs, _, _ := setup(t, &config.Config{Targets: testTargets})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Name == "list_targets" || tool.Name == "search_everywhere" {
			continue
		}
		schema := tool.InputSchema.(map[string]any)
		req := schema["required"].([]any)
		if req[0] != "target" || !slices.Contains(req, any("query")) {
			t.Errorf("%s: required = %v", tool.Name, req)
		}
		props := schema["properties"].(map[string]any)
		_, hasConfirm := props["confirm"]
		if wantConfirm := tool.Name == "update_dashboard" || tool.Name == "grafana_api_request"; hasConfirm != wantConfirm {
			t.Errorf("%s: confirm presente=%v, esperado=%v", tool.Name, hasConfirm, wantConfirm)
		}
	}
	for _, want := range []string{"list_targets", "search_everywhere", "search_dashboards", "update_dashboard"} {
		if !slices.Contains(names, want) {
			t.Errorf("falta la tool %s en %v", want, names)
		}
	}
}

func TestRoutingAndAliases(t *testing.T) {
	cs, calls, _ := setup(t, &config.Config{Targets: testTargets})
	out, isErr := call(t, cs, "search_dashboards", map[string]any{"target": "VITACURA", "query": "cpu"})
	if isErr || out != `prod-vitacura:search_dashboards:{"query":"cpu"}` {
		t.Fatalf("got %q (err=%v)", out, isErr)
	}
	if out, isErr := call(t, cs, "search_dashboards", map[string]any{"query": "cpu"}); !isErr || !strings.Contains(out, "list_targets") {
		t.Errorf("sin target debería fallar con ayuda, got %q", out)
	}
	if out, isErr := call(t, cs, "search_dashboards", map[string]any{"target": "nope", "query": "x"}); !isErr || !strings.Contains(out, "prod-vitacura") {
		t.Errorf("target inválido debería listar los válidos, got %q", out)
	}
	if got := calls.list(); len(got) != 1 {
		t.Errorf("llamadas upstream = %v", got)
	}
}

func TestWritePolicy(t *testing.T) {
	cs, calls, audit := setup(t, &config.Config{Targets: testTargets})
	cases := []struct {
		name    string
		tool    string
		args    map[string]any
		wantErr bool
	}{
		{"write en testing", "update_dashboard", map[string]any{"target": "testing-profesor", "query": "x"}, false},
		{"write en prod read", "update_dashboard", map[string]any{"target": "prod-vitacura", "query": "x"}, true},
		{"write en prod confirm sin confirm", "update_dashboard", map[string]any{"target": "prod-profesor", "query": "x"}, true},
		{"write en prod confirm con confirm", "update_dashboard", map[string]any{"target": "prod-profesor", "query": "x", "confirm": true}, false},
		{"api GET en prod read", "grafana_api_request", map[string]any{"target": "prod-vitacura", "query": "x", "method": "GET"}, false},
		{"api POST en prod read", "grafana_api_request", map[string]any{"target": "prod-vitacura", "query": "x", "method": "POST"}, true},
	}
	for _, c := range cases {
		out, isErr := call(t, cs, c.tool, c.args)
		if isErr != c.wantErr {
			t.Errorf("%s: isErr=%v, esperado %v (%s)", c.name, isErr, c.wantErr, out)
		}
		if strings.Contains(out, `"confirm"`) {
			t.Errorf("%s: confirm no debe llegar al upstream: %s", c.name, out)
		}
	}
	if got := calls.list(); len(got) != 3 {
		t.Errorf("se esperaban 3 llamadas upstream, hubo %d: %v", len(got), got)
	}
	var denied, allowed int
	for _, line := range strings.Split(strings.TrimSpace(audit.String()), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		switch e["decision"] {
		case "denied":
			denied++
		case "allowed":
			allowed++
		}
	}
	if denied != 3 || allowed != 2 {
		t.Errorf("auditoría: denied=%d allowed=%d\n%s", denied, allowed, audit)
	}
}

func TestSearchEverywhere(t *testing.T) {
	cs, _, _ := setup(t, &config.Config{Targets: testTargets})
	out, isErr := call(t, cs, "search_everywhere", map[string]any{"query": "mqtt", "env": "prod"})
	if isErr {
		t.Fatal(out)
	}
	if !strings.Contains(out, "target=prod-vitacura") || !strings.Contains(out, "target=prod-profesor") || strings.Contains(out, "testing-profesor") {
		t.Errorf("resultado inesperado:\n%s", out)
	}
}

func TestIncludeExclude(t *testing.T) {
	cs, _, _ := setup(t, &config.Config{Targets: testTargets, Tools: config.Tools{
		Include: []string{"search_dashboards", "update_dashboard", "user_info"},
		Exclude: []string{"update_dashboard"},
	}})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	want := []string{"list_targets", "search_dashboards", "search_everywhere", "user_info"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, esperado %v", names, want)
	}
}
