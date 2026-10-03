package gateway

import (
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cursor-cl/gateway-grafana/internal/config"
)

// Tools que mcp-grafana no marca como readOnly pero cuya escritura depende de los argumentos.
var argDependentTools = map[string]func(args map[string]any) bool{
	// Solo es escritura si el método no es GET/HEAD.
	"grafana_api_request": func(args map[string]any) bool {
		m, _ := args["method"].(string)
		switch strings.ToUpper(m) {
		case "", "GET", "HEAD":
			return false
		}
		return true
	},
	// Solo genera URLs; acortarlas crea un short-url pero no modifica recursos.
	"generate_deeplink": func(map[string]any) bool { return false },
}

// isWriteTool clasifica una tool a partir de las anotaciones del upstream.
// Si no hay anotaciones se asume escritura (fail closed).
func isWriteTool(t *mcp.Tool) bool {
	if _, ok := argDependentTools[t.Name]; ok {
		return true
	}
	return t.Annotations == nil || !t.Annotations.ReadOnlyHint
}

// isWriteCall decide si una llamada concreta escribe.
func isWriteCall(t *mcp.Tool, args map[string]any) bool {
	if f, ok := argDependentTools[t.Name]; ok {
		return f(args)
	}
	return isWriteTool(t)
}

// checkWrite aplica el modo del target a una llamada de escritura.
func checkWrite(t config.Target, tool string, confirmed bool) error {
	switch t.Mode {
	case config.ModeWrite:
		return nil
	case config.ModeConfirm:
		if confirmed {
			return nil
		}
		return fmt.Errorf("%s escribe en %q (%s), que requiere confirmación: muéstrale al usuario el cambio exacto, pídele aprobación explícita y reintenta con confirm=true", tool, t.ID, t.Env)
	default:
		return fmt.Errorf("%s escribe y %q (%s) es de solo lectura en el gateway; usa un target de testing o pide al usuario que cambie el mode del target", tool, t.ID, t.Env)
	}
}
