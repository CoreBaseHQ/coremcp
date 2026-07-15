# CoreMCP

[![CI](https://github.com/corebasehq/coremcp/workflows/CI/badge.svg)](https://github.com/corebasehq/coremcp/actions)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Go Version](https://img.shields.io/github/go-mod/go-version/corebasehq/coremcp)](https://golang.org/doc/devel/release.html)
[![Release](https://img.shields.io/github/v/release/corebasehq/coremcp)](https://github.com/corebasehq/coremcp/releases)

A Model Context Protocol (MCP) server, written in Go, that exposes SQL databases as MCP tools and prompts. It runs as a single static binary, embeds its drivers, and talks either stdio (for local MCP clients like Claude Desktop) or an outbound WebSocket (for remote operation behind NAT).

Currently ships with MSSQL (SQL Server 2000+, `Turkish_CI_AS` collation aware) and PostgreSQL adapters. Firebird is in progress; MySQL is on the roadmap.

## Status

- **Stable:** MSSQL adapter, PostgreSQL adapter, stdio transport, schema discovery, custom tools, NOLOCK / Turkish normalization middleware, WebSocket connect mode.
- **In progress:** Firebird adapter (factory currently returns a placeholder error).
- **Roadmap:** MySQL, HTTP transport, audit log, query result cache.

## Defaults

CoreMCP is read-only by default. Omitting `readonly` in a source config leaves SELECT-only mode active; you have to set `readonly: false` to enable `execute_procedure`. Even so, the recommended posture is a dedicated DB user with `SELECT` (and `EXECUTE` only on the procedures you intend to expose) — defense in depth rather than relying solely on the server-side guard.

## Install

### Binary

Download from the [Releases page](https://github.com/corebasehq/coremcp/releases) — `linux/amd64`, `linux/arm64`, `darwin/{amd64,arm64}`, `windows/amd64`.

One-line installer (Linux/macOS):

```bash
curl -fsSL https://get.corebasehq.com | sh
```

### Docker

```bash
docker pull y11t0/coremcp:latest
```

Multi-arch image (`linux/amd64`, `linux/arm64`).

### From source

Requires Go 1.23+.

```bash
git clone https://github.com/corebasehq/coremcp.git
cd coremcp
go build -o coremcp ./cmd/coremcp
```

## Configuration

`coremcp.yaml` in the working directory:

```yaml
server:
  name: "coremcp-agent"
  version: "0.1.0"
  transport: "stdio"
  port: 8080

logging:
  level: "info"
  format: "json"

sources:
  - name: "my_database"
    type: "mssql"
    dsn: "sqlserver://username:password@localhost:1433?database=mydb&encrypt=disable"
    readonly: true
    no_lock: true            # READ UNCOMMITTED isolation (WITH (NOLOCK) equivalent)
    normalize_turkish: true  # Turkish character + mojibake normalization
```

See [coremcp.example.yaml](coremcp.example.yaml) for a fuller example.

### DSN format

MSSQL:

```
sqlserver://username:password@host:port?database=dbname&encrypt=disable
```

PostgreSQL:

```
postgresql://username:password@host:port/dbname?sslmode=disable
```

Dummy adapter (for testing without a real DB):

```
dummy://test
```

### Source options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `name` | string | — | Unique source identifier |
| `type` | string | — | Adapter type: `mssql`, `postgres` (or `postgresql`), `rest`, `graphql`, `dummy` |
| `dsn` | string | — | Connection string |
| `readonly` | bool | `true` | SELECT-only at the config level. Set `false` explicitly to allow `execute_procedure`. |
| `no_lock` | bool | `false` | **(MSSQL only)** Run SELECTs under `READ UNCOMMITTED`. Equivalent to `WITH (NOLOCK)` on every table reference. Eliminates shared lock acquisition on busy OLTP. **Trade-off:** dirty reads possible. |
| `normalize_turkish` | bool | `false` | **(MSSQL only)** Two-way middleware. Outgoing: Turkish characters inside SQL string literals are folded to ASCII uppercase before the query is sent (`'Hüseyin'` → `'HUSEYIN'`). Incoming: Windows-1254 / Windows-1252 mojibake in result strings is auto-corrected. Intended for legacy Turkish ERP databases on `Turkish_CI_AS`. |

#### Example: MSSQL with NOLOCK

```yaml
sources:
  - name: "oltp_db"
    type: "mssql"
    dsn: "sqlserver://user:pass@localhost:1433?database=production&encrypt=disable"
    readonly: true
    no_lock: true
```

#### Example: legacy Turkish ERP

```yaml
sources:
  - name: "erp_db"
    type: "mssql"
    dsn: "sqlserver://user:pass@localhost:1433?database=LOGO&encrypt=disable"
    readonly: true
    no_lock: true
    normalize_turkish: true
```

How the Turkish middleware behaves:

| Model emits | Sent to DB | Why |
|-------------|------------|-----|
| `WHERE ADI = 'Hüseyin'` | `WHERE ADI = 'HUSEYIN'` | ERP stores names as uppercase ASCII |
| `WHERE SEHIR LIKE '%şeker%'` | `WHERE SEHIR LIKE '%SEKER%'` | `Ş` → `S` |
| `WHERE SEHIR = 'İstanbul'` | `WHERE SEHIR = 'ISTANBUL'` | `İ` → `I` |

Mojibake correction on inbound rows:

| DB returns | Fixed | Cause |
|------------|-------|-------|
| `GÐKHAN` | `GĞKHAN` | Win-1254 byte `0xD0` read as Win-1252 |
| `ÝSTANBUL` | `İSTANBUL` | Win-1254 byte `0xDD` read as Win-1252 |
| `ÞEHİR` | `ŞEHİR` | Win-1254 byte `0xDE` read as Win-1252 |

### Security configuration

```yaml
security:
  max_row_limit: 1000        # forced LIMIT cap
  enable_pii_masking: true
  pii_patterns:
    - name: "credit_card"
      pattern: '\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b'
      replacement: "****-****-****-****"
      enabled: true
    - name: "email"
      pattern: '\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b'
      replacement: "***@***.***"
      enabled: true
    - name: "turkish_id"
      pattern: '\b[1-9]\d{10}\b'
      replacement: "***********"
      enabled: true
```

What this enables:

- **T-SQL aware lexer.** Fail-closed custom tokeniser strips comments and string literals, then classifies the statement — only `SELECT` and `WITH` pass. `DROP`, `ALTER`, `UPDATE`, `DELETE`, `TRUNCATE`, `EXEC`, `OPENROWSET`, `SELECT…INTO` and similar are rejected before reaching the DB. Multi-statement payloads (any `;` outside strings/comments) are fatal — stacked-query attacks blocked dialect-independently. Chosen over third-party Go SQL parsers (xwb1989/sqlparser, vitess, cockroachdb) because they fail-closed on T-SQL hints and any "fall through to regex" relaxation is bypassable via `EX/**/EC` and similar tricks. Treat as one layer, not the only layer — pair with a least-privilege DB role.
- **Forced row cap.** `LIMIT` is appended (or wrapped) on every SELECT so a model never streams millions of rows back through the protocol.
- **PII masking.** Regex-based post-processing on result strings before they reach the client.

## Usage

CoreMCP has two operation modes.

### 1. Local (`serve`)

For local MCP clients (Claude Desktop, etc.):

```bash
coremcp serve --config coremcp.yaml
```

stdio is the default transport:

```bash
coremcp serve -t stdio
```

Claude Desktop config (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "coremcp": {
      "command": "/path/to/coremcp",
      "args": ["serve", "-c", "/path/to/coremcp.yaml"],
      "env": {}
    }
  }
}
```

### 2. Remote (`connect`)

`connect` opens an outbound WebSocket to a relay (typically CoreBase Cloud) and serves MCP traffic over it. The agent never accepts inbound connections, so it works from inside networks that don't allow inbound 443 (factory floors, corporate VPCs, hospital networks).

```bash
coremcp connect --server="wss://api.corebasehq.com/ws/agent" --token="sk_xxx"
```

Flags:

```
-s, --server string              Relay WebSocket URL (required)
-t, --token string               Authentication token (required)
-a, --agent-id string            Agent ID (auto-generated if omitted)
-r, --max-reconnect int          Max reconnect attempts (default 10; 0 = infinite)
-d, --reconnect-delay duration   Delay between reconnect attempts (default 5s)
```

Example, long-running:

```bash
./coremcp connect \
  --server="wss://api.corebasehq.com/ws/agent" \
  --token="sk_xxx" \
  --agent-id="site-istanbul-001" \
  --max-reconnect=0
```

Wire commands supported by the relay protocol:

- `run_sql` — execute SQL
- `get_schema` — dump cached schema
- `list_sources` — enumerate configured sources
- `health_check` — agent liveness
- `config_sync` — push updated source configs to the running agent

## Architecture

```
coremcp/
├── cmd/coremcp/       # CLI entry point
│   ├── main.go
│   ├── root.go
│   ├── serve.go       # stdio mode
│   └── connect.go     # WebSocket mode
├── pkg/
│   ├── adapter/       # Database adapters
│   │   ├── factory.go
│   │   ├── dummy/
│   │   └── mssql/
│   ├── config/
│   ├── core/          # Shared types, Source interface
│   ├── security/      # Query validation, PII masking
│   └── server/        # MCP server
└── coremcp.yaml
```

## Tools and prompts

### Built-in tools

#### `query_database`

Arbitrary SQL against a configured source.

- `source_name` (required)
- `query` (required)

#### `list_tables`

Tables with column counts, primary keys, foreign key counts.

- `source_name` (required)

#### `describe_table`

Full schema for one table: columns, types, nullability, PKs, FKs, column comments.

- `source_name` (required)
- `table_name` (required)

#### `list_views`

All views with column definitions.

- `source_name` (required)

#### `list_procedures`

Stored procedures with parameter names, types, modes (`IN`/`OUT`/`INOUT`), and a ready-to-copy example call.

- `source_name` (required)

#### `execute_procedure`

Calls a stored procedure with named parameters. **Only enabled when `readonly: false`.**

- `source_name` (required)
- `procedure_name` (required)
- `params` (optional) — JSON object of name/value pairs

Hardening:

- Procedure name validated against `^[a-zA-Z_][a-zA-Z0-9_#@.]*$`
- Parameter names validated (alphanumeric + underscore)
- Values bound via `sql.Named` — no string interpolation
- Rejected outright when source is `readonly: true`

Example:

```json
{
  "source_name": "erp_db",
  "procedure_name": "sp_CiroHesapla",
  "params": "{\"StartDate\":\"2024-01-01\",\"EndDate\":\"2024-12-31\"}"
}
```

### Custom tools

Define reusable parameterized queries as first-class MCP tools:

```yaml
custom_tools:
  - name: "get_daily_sales"
    description: "Daily sales summary for a given date"
    source: "production_db"
    query: "SELECT * FROM orders WHERE DATE(created_at) = '{{date}}'"
    parameters:
      - name: "date"
        description: "Date in YYYY-MM-DD format"
        required: true

  - name: "get_top_customers"
    description: "Top N customers by order count"
    source: "production_db"
    query: "SELECT user_id, COUNT(*) AS order_count FROM orders GROUP BY user_id ORDER BY order_count DESC LIMIT {{limit}}"
    parameters:
      - name: "limit"
        description: "Number of customers to return"
        required: true
        default: "10"
```

These get exposed to the model with their declared parameter schema, so the model can call them directly rather than re-deriving the SQL each turn.

### `database_schema` prompt

On startup CoreMCP connects to every configured source, scans tables / columns / keys / relationships, and extracts column comments (e.g. `MS_Description` on MSSQL). The result is exposed as a single MCP prompt that primes the model with schema context — including the comments — so it can write correct queries without manual schema dumps in every conversation.

## Adding adapters

1. Create `pkg/adapter/yourdb/`.
2. Implement `core.Source`.
3. Register in `pkg/adapter/factory.go`.

[pkg/adapter/dummy/dummy.go](pkg/adapter/dummy/dummy.go) is the minimum reference implementation.

## Roadmap

- [x] Schema discovery on startup
- [x] Column comments / descriptions
- [x] Built-in `list_tables` / `describe_table`
- [x] Custom parameterized tools
- [x] T-SQL aware lexer for query sanitization (fail-closed, multi-statement reject, no third-party parser)
- [x] PII masking
- [x] Forced row cap
- [x] WebSocket `connect` mode
- [x] Auto-reconnect
- [x] Remote config sync
- [x] NOLOCK / READ UNCOMMITTED per source (MSSQL)
- [x] Turkish character + mojibake middleware (MSSQL)
- [x] View and procedure discovery (`list_views`, `list_procedures`, `execute_procedure`)
- [x] PostgreSQL adapter
- [ ] Firebird adapter (in progress)
- [ ] MySQL adapter
- [ ] HTTP transport
- [ ] Query result cache
- [ ] Write operations (with explicit safety guards)
- [ ] Audit logging
- [ ] Multi-agent management
- [ ] Real-time monitoring

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security reports: [SECURITY.md](SECURITY.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE).

## Support

- [Report a bug](https://github.com/corebasehq/coremcp/issues/new?template=bug_report.md)
- [Request a feature](https://github.com/corebasehq/coremcp/issues/new?template=feature_request.md)
- Email: support@corebasehq.com

---

## About

CoreMCP is the open-source, on-prem gateway component of [CoreBase](https://corebasehq.com), an AI agent platform for your company's data. Chat with your databases and APIs directly, or let autonomous and event-triggered agents run multi-step automations across them — databases (SQL Server 2000+, PostgreSQL), REST and GraphQL APIs, and 50+ SaaS connectors.

CoreMCP is how those agents reach the systems behind your firewall, including the legacy and on-prem ones nothing else connects to: it runs on your own server, keeps database credentials local, and connects zero-trust — outbound port 443 only, no inbound ports. On top of that access, CoreBase layers Unified Context and Query Memory: the schema relationships, terminology, and proven query patterns that turn raw access into accurate answers.