# gateway-grafana

Servidor MCP único (en Go) que reemplaza un MCP por cada organización de Grafana.
Por detrás levanta un `mcp-grafana` por target, solo cuando hace falta, y enruta cada llamada
según el parámetro `target`.

```
Claude ──► gateway-grafana (1 MCP, ~66 tools)
              ├─ targets.yaml   catálogo: env, org, url, descripción, alias, mode
              ├─ política       read | confirm | write por target + auditoría JSONL
              └─ pool ──► mcp-grafana (stdio, bajo demanda) × N targets
```

## Qué hace

- **Un parámetro `target` en cada tool** (enum con los ids del catálogo). También acepta alias sin distinguir mayúsculas, como `vitacura` o `dq`.
- **`list_targets`**: muestra al modelo qué hay en cada org.
- **`search_everywhere`**: busca dashboards en todas las orgs, o en un entorno, en paralelo.
- **Instrucciones del servidor** generadas a partir del catálogo, con las descripciones de cada target.
- **Política de escritura** basada en las anotaciones `readOnlyHint` de mcp-grafana:
  - `read`: el gateway bloquea las escrituras y además el upstream corre con `--disable-write`.
  - `confirm`: solo se escribe si la llamada trae `confirm=true`, después de que el usuario aprueba.
  - `write`: escritura libre.
  - `grafana_api_request` cuenta como escritura solo si el método no es GET o HEAD.
- **Auditoría**: cada intento de escritura, permitido o denegado, se registra en `audit_log` (JSONL).

## Uso

```bash
go build -o bin/gateway-grafana .
cp .env.example .env        # completar los tokens
./bin/gateway-grafana -env-file .env -check   # prueba user_info en cada target
```

Registrar en Claude Code (ámbito usuario):

```bash
claude mcp add grafana --scope user -- \
  $PWD/bin/gateway-grafana -config $PWD/targets.yaml -env-file $PWD/.env
```

## Agregar una organización

1. Crear un service account en la org de Grafana y generar su token.
2. Agregar una entrada en `targets.yaml` (`id`, `env`, `org`, `url`, `token_env`, `mode`, `description`, `aliases`).
3. Agregar el token a `.env` y reiniciar la sesión de Claude Code.

Una buena `description` (qué datasources tiene, qué clientes o sensores, para qué se usa)
es lo que más ayuda al modelo a elegir bien el target.

## Desarrollo

```bash
go test -race ./...
```

Las pruebas usan upstreams MCP falsos en memoria (`internal/gateway/gateway_test.go`).
