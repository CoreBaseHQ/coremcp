# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.9] - 2026-05-26

### Security
- **Query validator rewritten as a T-SQL aware lexer.** The previous implementation combined `xwb1989/sqlparser` (MySQL dialect, unmaintained since 2018) with a regex denylist fallback. The fallback was bypassable via comment/whitespace tricks — `SELECT 1; EX/**/EC xp_cmdshell …` slipped past the regex because the engine sees `EX/**/EC` while SQL Server treats `/**/` as whitespace and executes the statement. Switching to vitess/cockroachdb parsers only moves the problem since every dialect-aware parser has corner cases where it cannot parse legitimate target-DB queries.
- **New validator posture** (`pkg/security/security.go`):
  1. Multi-statement payloads rejected — any `;` outside string literals or comments (other than a single trailing one) is fatal, blocking stacked-query attacks dialect-independently.
  2. Leading keyword classified after stripping comments and string literals — only `SELECT` and `WITH` (CTE) pass. Every other recognised statement keyword returns a specific error.
  3. Forbidden-in-body scan for `SELECT…INTO`, `OPENROWSET`, `OPENQUERY`, `OPENDATASOURCE` — the actual T-SQL write/exfiltration vectors that turn a SELECT-shaped statement into something dangerous.
- **Token-fusion protection:** `stripCommentsAndStrings` replaces stripped content with a single space so adjacent identifiers never fuse — `EX/**/EC` becomes `EX EC`, not `EXEC`.
- **`xwb1989/sqlparser` dependency removed** from `go.mod` / `go.sum`. No third-party SQL parser is required.

### Added
- **Bypass test suite** (`pkg/security/security_test.go`): explicit coverage for the bypass classes that motivated the rewrite — stacked statements after a `SELECT` prefix, `EX/**/EC` split by inline comment, `EXEC` hidden behind a block-comment prefix, `DROP` after a `--` line-comment terminator, stacked statements via single-quoted decoy, unparseable garbage. Also covers `SELECT…INTO` (T-SQL temp table, MySQL `OUTFILE`/`DUMPFILE`), `OPENROWSET`/`OPENQUERY`/`OPENDATASOURCE`, and CTE exfil via `SELECT…INTO`.
- **False-positive guards** in the test suite: forbidden tokens hidden inside `/* */` or single-quoted string literals do NOT trigger rejection (they are stripped before the body scan). Verifies the stripped-then-classified design.
- **`firstKeyword` and `stripCommentsAndStrings` unit tests** covering paren-wrapped UNION, T-SQL `SELECT TOP`, bracketed identifiers (`[dbo].[users]`), comment prefixes, and adjacent-token-fusion edge cases.

### Changed
- `NewQueryValidator(allowedKeywords, blockedKeywords)` signature preserved for backwards compatibility — the cloud control plane and existing `coremcp.yaml` files still reference these fields, but they are no longer consulted by the validator. The slices are retained on the struct (with `//nolint:unused`) so `config_sync` payloads continue to deserialise without breakage.
- Validator error messages are now operation-specific (`"write operations are not allowed (UPDATE)"`, `"DDL operations are not allowed (DROP)"`, `"procedure execution must use the execute_procedure tool, not inline EXEC"`, etc.) instead of a generic blocked-keyword string.

### Notes
- CTE bodies are NOT introspected for nested DML keywords. For PostgreSQL, a CTE may legally contain `DELETE … RETURNING` — the line of defence is the DB user's permission set (the README requires a SELECT-only user, and the cloud-direct adapter additionally pins the session to `default_transaction_read_only`). MSSQL CTEs are SELECT-only at the grammar level, so the primary target is unaffected.

## [0.4.8] - 2026-05-26

### Fixed
- **REST/GraphQL sources configured via `base_url`:** `serve` now calls `SourceConfig.BuildDSN()` instead of passing the raw `dsn` field, so REST and GraphQL sources configured per the documented schema (`base_url` + `api_key` + `headers`) actually connect. Previously the adapter received an empty DSN.
- **`connect` SQL parameter binding:** `executeSQL` now converts the protocol's `params` map to `sql.NamedArg` before invoking `ExecuteQuery`. Previously the raw `map[string]interface{}` was forwarded to `database/sql`, which rejected it at runtime for any `run_sql` command carrying non-empty params.
- **Schema cache data race:** `MCPServer.schemaContext` and `MCPServer.tableSchemas` are now guarded by an `sync.RWMutex`. `LoadSchemas` builds into a local map and swaps it in atomically, so `describe_table` and the `database_schema` prompt can no longer observe a partially populated cache while the background schema load is in flight.
- **`config_sync` lock interleaving (`connect` mode):** `handleConfigSync` now serialises through a dedicated `syncMu`, snapshots the live source set, and closes detached sources after releasing `c.mu`. Concurrent sync messages can no longer interleave teardown/setup phases, and a slow `Close` no longer blocks `executeSQL`/`getSchema` readers.
- **UNION row cap:** `QueryModifier.AddRowLimit` now wraps `UNION` queries in a `SELECT * FROM (…) AS _capped LIMIT N` subquery so the row cap applies to the combined result set. Previously the limit affected only the trailing `SELECT` after the MSSQL `LIMIT`→`TOP` rewrite.
- **T-SQL passthrough:** `AddRowLimit` now detects MSSQL-shaped queries up-front and skips the `xwb1989/sqlparser` AST path entirely, preventing brackets, `WITH (NOLOCK)` hints, and `N'…'` literals from being mangled when re-emitted as MySQL syntax.

### Changed
- **`connect` mode goroutine cap:** In-flight command handlers are now bounded by a per-connection semaphore (32) so a fast-pushing or misbehaving server cannot exhaust memory by spawning unbounded goroutines.
- **Reconnect jitter:** Replaced the `time.Now().UnixNano()%100` jitter source with `math/rand/v2`, giving uncorrelated ±10% spread when many agents reconnect simultaneously.
- **MSSQL `Close`:** Documented that the context parameter is accepted only to satisfy `core.Source`; `database/sql`'s `Close` is bounded and does not accept a context.
- **Server version auto-detected:** `server.version` no longer needs to be hand-edited in `coremcp.yaml`. When omitted, the MCP server advertises the binary version compiled in via `-ldflags` (the same string shown by `coremcp version`). Setting `server.version` explicitly still overrides the default for users who need a custom advertised version.

## [0.4.7] - 2026-05-23

### Changed
- **CoreMCP CLI:** Refactored command lifecycle to use `RunE` instead of `log.Fatal` to allow deferred cleanup execution.
- **Source Initialization:** Replaced arbitrary `time.Sleep` with a robust `sync.WaitGroup` to ensure schemas finish loading before starting the MCP server.
- **Optimization:** Generic SQL `QueryValidator` regex patterns are now pre-compiled once at startup, preventing runaway CPU/OOM spikes on high throughput constraints.

### Fixed
- **MSSQL N+1 Schema Queries:** Drastically improved startup performance by moving from iterative row queries to bulk `INFORMATION_SCHEMA` queries stitched via in-memory maps in `GetSchema`, `GetViews`, and `GetProcedures`.
- **MSSQL View Resolution:** Fixed a bug where view names dropped their parent schema context across identically named views in different schemas.
- **Strict MSSQL Row Limiting:** Added dialect awareness for `TOP` and `FETCH NEXT` to gracefully cap rows without generating broken MySQL-style `LIMIT 1000` queries on MSSQL servers.
- **Cursor Safety:** Enforced strict `rows.Err()` checks across all database iteration loops to catch fatal cursor errors.
- **Deadlock Prevention:** Added `context.WithTimeout` logic attached to a new `--command-timeout` flag enforcing query execution deadlines.
- **Error Wrapping:** Modernized error propagations with Go's standard `%w` format for unwrapability.

## [0.4.6] - 2026-05-02

### Added
- **REST API adapter (`type: rest`):** Connect to any REST/OpenAPI-based service as a CoreMCP source. Discovers endpoints from an OpenAPI 3.0 spec at startup and exposes them as MCP tools. Configure with `base_url`, `api_key`, `spec_url`, `headers`, and `timeout` — no raw DSN required; `BuildDSN()` compiles a `rest://` connection string automatically.
- **GraphQL adapter (`type: graphql`):** Connect to GraphQL APIs with automatic schema introspection. Maps every query and mutation to an MCP tool. Configure with `base_url`, `api_key`, `headers`, and `timeout`; DSN is built as `graphql://` automatically.
- **Semantic context in `get_schema` responses:** The CoreBase control plane can now push business-layer annotations (table/column descriptions, glossary terms, virtual JOIN hints) to the agent via `config_sync`. These are stored per-source in `ConnectClient` and attached to every `get_schema` response so the LLM receives full semantic enrichment without a round-trip.
- **`SourceConfig.BuildDSN()`:** New method on `SourceConfig` that constructs the adapter DSN from structured fields (`base_url`, `api_key`, `spec_url`, `headers`, `timeout`) for REST and GraphQL sources. Existing database sources and configs that already provide a `dsn` field are unaffected.

### Changed
- `coremcp.example.yaml` updated with commented REST and GraphQL source examples, updated DSN format reference, and server version bumped to `0.4.6`.
- `adapter.NewSource` error message changed to lower-case (`"unsupported DB type: %s"`) for consistency with Go error conventions.

## [0.4.5] - 2026-03-26

### Changed
- **`readonly` now defaults to `true`**: Sources without an explicit `readonly` field are now read-only by default. Previously the zero value for bool meant omitting the field silently allowed write operations. Set `readonly: false` explicitly to enable `execute_procedure`.

## [0.4.4] - 2026-03-22

### Fixed
- **MSSQL TOP/LIMIT conflict:** Queries containing `SELECT TOP N` no longer receive a spurious `LIMIT N` suffix from `QueryModifier`, which caused SQL Server to return `Incorrect syntax near 'LIMIT'`.
- **MCP initialize timeout:** Database schemas are now loaded in a background goroutine after server startup, preventing the MCP `initialize` handshake from timing out on large databases.

### Security
- **Custom tool parameter validation:** Replaced metacharacter denylist with strict type-based allowlisting. Custom tool parameters now declare a `type` field (`integer`, `number`, `date`, `identifier`, `string`); values are validated and re-serialised before interpolation — eliminating logic-injection payloads such as `1 OR 1=1`.

### Changed
- `coremcp.example.yaml` updated with `type` field examples for custom tool parameters.

## [0.4.3] - 2026-03-05

### Added
- **Glama registry support:** Added `glama.json` for listing CoreMCP on the Glama MCP registry.

### Fixed
- **Docker default config:** Docker image now uses `coremcp.example.yaml` as the default configuration instead of requiring a mounted config file, making the out-of-the-box experience work correctly.

## [0.4.2] - 2026-02-18

### Fixed
- **NOLOCK transaction error on `go-mssqldb`:** `executeQueryReadUncommitted` was opening the
  transaction with `sql.TxOptions{Isolation: sql.LevelReadUncommitted, ReadOnly: true}`. The
  `go-mssqldb` driver does not support the `ReadOnly` transaction flag and returned:
  `"read-only transactions are not supported"`. Removed `ReadOnly: true` — `READ UNCOMMITTED`
  isolation alone is sufficient: it prevents shared lock acquisition and the transaction is always
  rolled back via `defer tx.Rollback()` so no data is ever modified.

## [0.4.1] - 2026-02-18

### Fixed
- **Config file not found when running outside project directory:** `LoadConfig` was passing the
  `--config` value to `viper.AddConfigPath()` which treats it as a *directory* path. When Claude
  Desktop (or any caller) passes a full file path such as `/home/user/coremcp/coremcp.yaml`, Viper
  failed to locate the file and silently fell back to the built-in dummy source.
  - `LoadConfig` now uses `os.Stat` to distinguish between a file path and a directory path:
    - **Full file path** → `viper.SetConfigFile(abs)` — exact load, working-directory independent
    - **Directory path** → `viper.AddConfigPath(dir)` — searches for `coremcp.yaml` inside that directory
    - **Default (no `--config`)** → searches the directory of the running binary first, then CWD
  - The default value of the `--config` flag is now computed at startup via `defaultConfigPath()`,
    which checks for `coremcp.yaml` next to the binary before falling back to `"coremcp.yaml"` (CWD-relative)
- **Single source failure crashing the entire server:** A failed `Connect()` call on one database
  source triggered `log.Fatalf`, killing the process before any tools were registered. Changed to
  `log.Printf` + `continue` so other sources remain available when one is unreachable.

## [0.4.0] - 2026-02-18

### Added
- **NOLOCK / READ UNCOMMITTED Support (MSSQL):** New per-source `no_lock` config option
  - When `no_lock: true`, all SELECT queries are executed inside a `READ UNCOMMITTED` transaction — functionally identical to adding `WITH (NOLOCK)` to every table reference
  - Prevents shared lock acquisition on MSSQL, reducing contention on busy OLTP databases
  - Transaction is always rolled back after each read; no data is ever modified
  - Trade-off: may return dirty (uncommitted) data — use only on reporting / AI-assistant workloads
  - Supported in both `serve` (local) mode and `connect` (remote/WebSocket) mode
  - New field added to `SourceConfig` (`no_lock: bool`) and `RemoteSource` (`no_lock: bool` in JSON)
- **Turkish Character Normalization Middleware (MSSQL):** New per-source `normalize_turkish` config option
  - Solves the "collation war" problem in legacy Turkish ERP databases (`Turkish_CI_AS` / Windows-1254)
  - **Outgoing query normalization:** Turkish characters inside SQL single-quoted string literals are automatically converted to their ASCII uppercase equivalents before the query is sent
    - Example: `WHERE ADI = 'Hüseyin'` → `WHERE ADI = 'HUSEYIN'`
    - Example: `WHERE SEHIR LIKE '%şeker%'` → `WHERE SEHIR LIKE '%SEKER%'`
    - Only string literals are modified — SQL keywords, column names and query structure are untouched
    - Mapping: `İıŞşGğğÜüÖöÇç` → `I I S S G G U U O O C C` (plus circumflex variants `ÂâÎîÛû`)
  - **Incoming result fix (mojibake correction):** Common Windows-1254 bytes misread as Windows-1252 are automatically corrected in result strings
    - `Ð` → `Gğ`, `Ý` → `İ`, `Þ` → `Ş`, `ð` → `ğ`, `ý` → `ı`, `þ` → `ş`
  - New package `pkg/turkish` with exported functions: `NormalizeSQLLiterals`, `ToASCIIUpper`, `FixMojibake`, `FixResultValue`
  - Works together with `no_lock: true` for maximum compatibility on legacy OLTP databases
  - Supported in both `serve` and `connect` modes
- **View & Stored Procedure Support (MSSQL):** AI agents can now discover and execute views and stored procedures
  - New `Source` interface methods: `GetViews`, `GetProcedures`, `ExecuteProcedure`
  - `GetViews` — fetches all database views with their column definitions via `INFORMATION_SCHEMA.VIEWS`
  - `GetProcedures` — fetches all stored procedures with parameter names, types, and modes (`IN`/`OUT`/`INOUT`) via `INFORMATION_SCHEMA.ROUTINES` + `INFORMATION_SCHEMA.PARAMETERS`
  - `ExecuteProcedure` — executes a stored procedure safely:
    - Procedure name is validated against `^[a-zA-Z_][a-zA-Z0-9_#@.]*$` (SQL injection prevention)
    - All parameter values are passed as named SQL parameters (`sql.Named`) — no string interpolation
    - Individual parameter names are also validated against `^[a-zA-Z_][a-zA-Z0-9_]*$`
    - Blocked entirely when the source is `readonly: true`
  - **Three new MCP tools** exposed to AI agents:
    - `list_views` — lists views with columns in markdown table format
    - `list_procedures` — lists stored procedures with parameters and an inline example call hint
    - `execute_procedure` — runs a procedure; accepts `params` as a JSON string e.g. `{"StartDate":"2024-01-01","Limit":"10"}`
  - Views and stored procedures are also included in the schema context built by `LoadSchemas`, so agents see them automatically without calling a tool
  - Turkish mojibake correction (`normalize_turkish`) is applied to procedure result sets as well
  - Supported in both `serve` and `connect` modes
- **SQL Server Version Compatibility:** Automatic version detection and query adaptation for legacy SQL Server deployments
  - On `Connect()`, CoreMCP queries `SERVERPROPERTY('ProductVersion')` and stores the major version number
  - `Name()` now reports the detected release (e.g. `"MSSQL (SQL Server 2008/2008 R2)"`) in logs and schema context so the AI knows which SQL dialect to generate
  - **SQL Server 2000 (v8) compatibility:** all schema-discovery queries branch to legacy system tables:
    - `GetSchema` uses `sysobjects WHERE xtype = 'U'` instead of `INFORMATION_SCHEMA.TABLES`
    - Column query uses `INFORMATION_SCHEMA.COLUMNS` only (no `sys.extended_properties` join — column descriptions silently omitted)
    - FK query uses `sysforeignkeys` / `COL_NAME()` instead of `sys.foreign_keys` / `sys.columns`
  - **LIMIT → TOP rewrite (all versions):** `security.QueryModifier` uses MySQL-dialect sqlparser and may inject `LIMIT N`; the MSSQL adapter now rewrites this to `SELECT TOP N` before execution
  - **OFFSET FETCH → TOP rewrite (pre-2012):** AI-generated `OFFSET x ROWS FETCH NEXT N ROWS ONLY` clauses are automatically stripped and replaced with `SELECT TOP N` on SQL Server 2008/2008 R2 and older (major version < 11)
  - Version constants: 8=2000, 9=2005, 10=2008/R2, 11=2012, 12=2014, 13=2016, 14=2017, 15=2019, 16=2022

### Changed
- `adapter.NewSource` signature updated to accept `noLock bool` and `normalizeTurkish bool` parameters
- `mssql.New` signature updated to accept `noLock bool` and `normalizeTurkish bool` parameters
- Startup log now reports `[NoLock: true/false, NormalizeTurkish: true/false]` alongside source details

## [0.3.0] - 2026-02-16

### Added
- **Remote Connect Mode**: WebSocket-based cloud connectivity for SaaS deployments
  - New `connect` command for remote management
  - WebSocket client implementation with gorilla/websocket
  - Authentication with API tokens
  - Support for remote commands: `run_sql`, `get_schema`, `list_sources`, `health_check`
  - Auto-Reconnection Logic: Configurable retry mechanism with exponential backoff
  - Remote Configuration Sync: Update database connections without redeployment
  - Heartbeat/ping-pong for connection monitoring
  - Perfect for factory deployments (no inbound ports required!)
  - Agent-initiated outbound connections only
  - Graceful shutdown handling

### Changed
- Architecture updated to support both stdio (local) and WebSocket (remote) modes
- README updated with comprehensive `connect` mode documentation
- Removed emojis from log messages for professional enterprise-grade logging
- Log messages now use standard prefixes: [INFO], [ERROR], [WARN], [DEBUG]

## [0.2.1] - 2026-02-15

### Fixed
- **Code Quality**: Fixed golangci-lint errcheck warnings in test files
  - Added proper error handling for `src.Connect()` calls in all test functions
  - Added error checks for `mcpSrv.LoadSchemas()` calls in schema tests
  - Improved test reliability with explicit error validation

## [0.2.0] - 2026-02-15

### Added
- **Enterprise-Grade Security Features:**
  - **AST-Based Query Sanitization**: Uses sqlparser library for deep SQL analysis
    - Blocks dangerous operations: INSERT, UPDATE, DELETE, DROP, ALTER, TRUNCATE, EXEC, MERGE, etc.
    - Allows only SELECT and WITH (CTE) queries
    - Validates queries at AST level, not just regex
  - **PII Data Masking**: Automatically masks personally identifiable information
    - Built-in patterns: Credit cards, emails, SSNs, Turkish IDs, IBANs, phone numbers
    - Configurable custom patterns via regex
    - Enable/disable per pattern
    - Enterprise-ready for GDPR/KVKK compliance
  - **Automatic Row Limiting**: Prevents database overload
    - Configurable max row limit (default: 1000)
    - Automatically adds LIMIT clause to queries
    - Preserves existing LIMIT if present
- **Dynamic Tool Generation:**
  - New `list_tables` tool: Lists all tables in a database with summary information
  - New `describe_table` tool: Shows detailed schema for a specific table
  - Custom tool support: Define reusable SQL queries as MCP tools in config
  - Parameter substitution in custom queries using `{{parameter}}` syntax
- **Automatic Schema Discovery:** CoreMCP now automatically scans database tables, columns, primary keys, and foreign keys on startup
- **Column Comments/Descriptions Support:** Extracts and presents database column comments (e.g., MS_Description in MSSQL) to AI for better context
- **Schema Context Prompt:** New `database_schema` MCP prompt that provides complete database structure to Claude automatically
- **Enhanced Schema Types:** 
  - New `ColumnInfo` type with name, data type, nullable flag, and description
  - New `ForeignKey` type with full relationship information
  - Enhanced `TableSchema` with primary keys array
- **MSSQL Extended Support:**
  - Automatic extraction of column descriptions from MS_Description extended properties
  - Full primary key discovery
  - Complete foreign key relationship mapping

### Changed
- **Query Execution**: Now uses security validator and row limiter for all queries
- **Config Structure**: Added `security` section with PII masking and row limit configuration
- **MCP Server**: Added `list_tables` and `describe_table` handlers
- **Config Structure**: Added `custom_tools` section for defining custom query tools
- **MSSQL Adapter:** Enhanced `GetSchema()` to retrieve complete table metadata including types, constraints, and relationships
- **MCP Server:** Added automatic schema loading during startup via `LoadSchemas()` method
- **Core Types:** Updated `TableSchema` structure to support rich metadata

### Security
- **BREAKING**: All queries now validated with AST parser (blocks write operations)
- **BREAKING**: Automatic LIMIT clause added to all SELECT queries (configurable)
- PII masking can be enabled via `security.enable_pii_masking` in config

## [0.1.0] - 2026-02-14

### Added
- **Core Server:** Implemented Model Context Protocol (MCP) server over stdio transport.
- **Database Support:**
  - Added MSSQL (Microsoft SQL Server) adapter with full query execution and schema discovery.
  - Added Dummy adapter for testing and demonstration purposes.
- **Configuration:**
  - YAML-based configuration system (`coremcp.yaml`).
  - Environment variable support with COREMCP_ prefix.
  - Support for multiple database sources in single config.
- **CLI:**
  - `serve` command to start the MCP agent.
  - Version flag support.
  - Configurable transport (stdio/http).
- **DevOps & CI/CD:**
  - GitHub Actions pipeline for automated testing and linting.
  - `Makefile` for cross-platform builds (Linux, macOS, Windows, ARM64).
  - `Dockerfile` for containerized deployment.
  - Docker Compose configuration for easy local testing.
- **Documentation:**
  - Comprehensive `README.md` with architecture diagrams.
  - `CONTRIBUTING.md` guide for open-source contributors.
  - `SECURITY.md` policy for responsible disclosure.
  - `CODE_OF_CONDUCT.md` for community standards.
  - Example configuration files.

### Security
- Implemented strict read-only mode for database connections.
- SQL query validation to prevent destructive operations (INSERT, UPDATE, DELETE, DROP, etc.).
- Whole-word matching for SQL keyword detection to avoid false positives.
- Support for SELECT and WITH (CTE) queries only in read-only mode.

### Fixed
- **Critical:** Fixed JSON-RPC protocol corruption by redirecting all adapter debug output to stderr instead of stdout.
  - Dummy adapter now correctly uses `os.Stderr` for all log messages.
  - Ensures stdio transport compatibility with Claude Desktop and other MCP clients.

## [0.0.1] - 2026-02-13

### Added
- Initial repository initialization.
- Project structure and Go module setup.
- Basic license (Apache 2.0) and gitignore files.
- Core package interfaces and types.

[Unreleased]: https://github.com/corebasehq/coremcp/compare/v0.4.8...HEAD
[0.4.8]: https://github.com/corebasehq/coremcp/compare/v0.4.7...v0.4.8
[0.4.7]: https://github.com/corebasehq/coremcp/compare/v0.4.6...v0.4.7
[0.4.6]: https://github.com/corebasehq/coremcp/compare/v0.4.5...v0.4.6
[0.4.5]: https://github.com/corebasehq/coremcp/compare/v0.4.4...v0.4.5
[0.4.4]: https://github.com/corebasehq/coremcp/compare/v0.4.3...v0.4.4
[0.4.3]: https://github.com/corebasehq/coremcp/compare/v0.4.2...v0.4.3
[0.4.2]: https://github.com/corebasehq/coremcp/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/corebasehq/coremcp/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/corebasehq/coremcp/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/corebasehq/coremcp/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/corebasehq/coremcp/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/corebasehq/coremcp/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/corebasehq/coremcp/compare/v0.0.1...v0.1.0
[0.0.1]: https://github.com/corebasehq/coremcp/releases/tag/v0.0.1