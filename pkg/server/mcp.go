// Package server implements the MCP (Model Context Protocol) server for database operations.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/corebasehq/coremcp/pkg/core"
	"github.com/corebasehq/coremcp/pkg/security"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mark3labs/mcp-go/server"
)

// safeIdentifierRe matches safe SQL identifiers (procedure/object names).
var safeIdentifierRe = regexp.MustCompile(`^[\w.\[\]]+$`)

// dateRe matches YYYY-MM-DD formatted dates.
var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// identifierRe matches simple SQL identifiers (letters, digits, underscores).
var identifierRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// ToolParam describes a single typed parameter accepted by a custom tool.
// Type drives the validation applied before the value is interpolated into the
// query template, providing the strongest protection against SQL injection.
//
// Supported types:
//   - "integer"    — value must parse as a 64-bit integer; interpolated bare (no quotes needed)
//   - "number"     — value must parse as a 64-bit float; interpolated bare
//   - "date"       — value must match YYYY-MM-DD; interpolated bare (use quotes in template)
//   - "identifier" — value must match ^[A-Za-z0-9_]+$; safe for unquoted interpolation
//   - "string"     — default; single quotes are escaped ('→”) for safe interpolation inside
//     SQL string literals — always wrap {{param}} in quotes in the template
type ToolParam struct {
	Name     string
	Type     string // "string" | "integer" | "number" | "date" | "identifier"
	Required bool
	Default  string
}

// validateAndCoerceParam validates value against the declared type and returns a
// safe string representation for direct SQL interpolation.
//
// For numeric types the value is parsed and re-stringified so arbitrary text
// can never slip through.  For "string" single quotes are doubled (SQL standard
// escaping), making the value safe when wrapped in quotes inside the template.
func validateAndCoerceParam(value, paramType string) (string, error) {
	switch paramType {
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return "", fmt.Errorf("expected integer, got %q", value)
		}
		return strconv.FormatInt(n, 10), nil
	case "number":
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return "", fmt.Errorf("expected number, got %q", value)
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	case "date":
		if !dateRe.MatchString(strings.TrimSpace(value)) {
			return "", fmt.Errorf("expected date in YYYY-MM-DD format, got %q", value)
		}
		return strings.TrimSpace(value), nil
	case "identifier":
		trimmed := strings.TrimSpace(value)
		if !identifierRe.MatchString(trimmed) {
			return "", fmt.Errorf("expected identifier (letters, digits, underscores only), got %q", value)
		}
		return trimmed, nil
	case "string", "": // Escape single quotes for safe SQL string-literal interpolation.
		return strings.ReplaceAll(value, "'", "''"), nil
	default: // Reject unknown/unsupported parameter types to avoid silently disabling validation.
		return "", fmt.Errorf("unknown parameter type %q; supported types: integer, number, date, identifier, string", paramType)
	}
}

// MCPServer represents the MCP server instance that handles database queries.
type MCPServer struct {
	mcpServer      *server.MCPServer
	sources        map[string]sourceEntry
	schemaContext  string                        // Pre-built schema context for AI
	customTools    map[string]customToolEntry    // Custom tools from config
	tableSchemas   map[string][]core.TableSchema // Cached table schemas per source
	queryValidator *security.QueryValidator      // SQL query validator
	piiMasker      *security.PIIMasker           // PII data masker
	queryModifier  *security.QueryModifier       // Query modifier for row limits
}

type customToolEntry struct {
	sourceName string
	query      string
	parameters []ToolParam
}

type sourceEntry struct {
	source   core.Source
	readOnly bool
}

// NewMCPServer creates a new MCP server instance with the specified name and version.
// It automatically registers the query_database tool for executing SQL queries.
func NewMCPServer(name, version string) *MCPServer {
	s := server.NewMCPServer(name, version)

	ms := &MCPServer{
		mcpServer:     s,
		sources:       make(map[string]sourceEntry),
		schemaContext: "",
		customTools:   make(map[string]customToolEntry),
		tableSchemas:  make(map[string][]core.TableSchema),
		// Security components will be initialized via ConfigureSecurity()
		queryValidator: security.NewQueryValidator(nil, nil),
		queryModifier:  security.NewQueryModifier(1000),
	}

	tool := mcp.NewTool("query_database",
		mcp.WithDescription("Executes a SQL query on a specified database source. Use this to retrieve data."),
		mcp.WithString("source_name", mcp.Required(), mcp.Description("The name of the database source defined in config (e.g., 'mydb')")),
		mcp.WithString("query", mcp.Required(), mcp.Description("The SQL query to execute (e.g., 'SELECT * FROM users')")),
	)

	s.AddTool(tool, ms.handleQueryDatabase)

	// Add list_tables tool
	listTablesTool := mcp.NewTool("list_tables",
		mcp.WithDescription("Lists all available tables in a database source with their column counts"),
		mcp.WithString("source_name", mcp.Required(), mcp.Description("The name of the database source")),
	)
	s.AddTool(listTablesTool, ms.handleListTables)

	// Add describe_table tool
	describeTableTool := mcp.NewTool("describe_table",
		mcp.WithDescription("Shows detailed schema information for a specific table including columns, types, keys, and relationships"),
		mcp.WithString("source_name", mcp.Required(), mcp.Description("The name of the database source")),
		mcp.WithString("table_name", mcp.Required(), mcp.Description("The name of the table to describe")),
	)
	s.AddTool(describeTableTool, ms.handleDescribeTable)

	// Add prompt for database schema context
	prompt := mcp.NewPrompt("database_schema",
		mcp.WithPromptDescription("Shows the complete database schema with tables, columns, types, and relationships"),
	)

	s.AddPrompt(prompt, ms.handleSchemaPrompt)

	// Add list_views tool
	listViewsTool := mcp.NewTool("list_views",
		mcp.WithDescription("Lists all database views in a source with their columns. Views are virtual tables based on stored queries."),
		mcp.WithString("source_name", mcp.Required(), mcp.Description("The name of the database source")),
	)
	s.AddTool(listViewsTool, ms.handleListViews)

	// Add list_procedures tool
	listProcsTool := mcp.NewTool("list_procedures",
		mcp.WithDescription("Lists all stored procedures in a database source with their parameters. Use 'execute_procedure' to run one."),
		mcp.WithString("source_name", mcp.Required(), mcp.Description("The name of the database source")),
	)
	s.AddTool(listProcsTool, ms.handleListProcedures)

	// Add execute_procedure tool
	execProcTool := mcp.NewTool("execute_procedure",
		mcp.WithDescription("Executes a stored procedure by name with optional parameters. Parameters are passed as a JSON object string. Only available on non-read-only sources."),
		mcp.WithString("source_name", mcp.Required(), mcp.Description("The name of the database source")),
		mcp.WithString("procedure_name", mcp.Required(), mcp.Description("The stored procedure name (e.g. 'sp_CiroHesapla')")),
		mcp.WithString("params", mcp.Description(`Optional JSON object of parameter name/value pairs. Example: {"StartDate":"2024-01-01","Limit":"10"}`)),
	)
	s.AddTool(execProcTool, ms.handleExecuteProcedure)

	return ms
}

// AddSource registers a database source with the given name.
// This makes the source available for query execution via the MCP protocol.
func (ms *MCPServer) AddSource(name string, src core.Source, readOnly bool) {
	ms.sources[name] = sourceEntry{
		source:   src,
		readOnly: readOnly,
	}
}

// ConfigureSecurity initializes security components with config values.
func (ms *MCPServer) ConfigureSecurity(maxRowLimit int, enablePIIMasking bool, piiPatterns []security.MaskPattern, allowedKeywords, blockedKeywords []string) error {
	// Configure query validator
	ms.queryValidator = security.NewQueryValidator(allowedKeywords, blockedKeywords)

	// Configure query modifier for row limiting
	ms.queryModifier = security.NewQueryModifier(maxRowLimit)

	// Configure PII masker
	patterns := piiPatterns
	if len(patterns) == 0 && enablePIIMasking {
		// Use default patterns if none provided
		patterns = security.DefaultPIIPatterns()
	}

	masker, err := security.NewPIIMasker(patterns, enablePIIMasking)
	if err != nil {
		return fmt.Errorf("failed to initialize PII masker: %w", err)
	}
	ms.piiMasker = masker

	return nil
}

// LoadSchemas loads database schemas from all sources and builds the context prompt.
// This should be called after all sources are added and connected.
func (ms *MCPServer) LoadSchemas(ctx context.Context) error {
	var contextBuilder strings.Builder
	contextBuilder.WriteString("=== DATABASE SCHEMA CONTEXT ===\n\n")
	contextBuilder.WriteString("You have access to the following database sources and their schemas:\n\n")

	for name, entry := range ms.sources {
		contextBuilder.WriteString(fmt.Sprintf("## Source: %s (%s)\n", name, entry.source.Name()))
		if entry.readOnly {
			contextBuilder.WriteString("(Read-Only Mode)\n")
		}
		contextBuilder.WriteString("\n")

		schemas, err := entry.source.GetSchema(ctx)
		if err != nil {
			contextBuilder.WriteString(fmt.Sprintf("⚠️  Error loading schema: %v\n\n", err))
			continue
		}

		// Cache schemas for use by describe_table without re-querying
		ms.tableSchemas[name] = schemas

		if len(schemas) == 0 {
			contextBuilder.WriteString("No tables found.\n\n")
			continue
		}

		for _, table := range schemas {
			contextBuilder.WriteString(fmt.Sprintf("### Table: %s\n", table.Name))

			// Primary Keys
			if len(table.PrimaryKeys) > 0 {
				contextBuilder.WriteString(fmt.Sprintf("Primary Key(s): %s\n", strings.Join(table.PrimaryKeys, ", ")))
			}

			// Columns
			contextBuilder.WriteString("Columns:\n")
			for _, col := range table.Columns {
				nullable := ""
				if col.IsNullable {
					nullable = " (nullable)"
				}
				contextBuilder.WriteString(fmt.Sprintf("  - %s: %s%s", col.Name, col.DataType, nullable))
				if col.Description != "" {
					contextBuilder.WriteString(fmt.Sprintf(" -- %s", col.Description))
				}
				contextBuilder.WriteString("\n")
			}

			// Foreign Keys
			if len(table.ForeignKeys) > 0 {
				contextBuilder.WriteString("Foreign Keys:\n")
				for _, fk := range table.ForeignKeys {
					contextBuilder.WriteString(fmt.Sprintf("  - %s → %s.%s (%s)\n",
						fk.ColumnName, fk.ReferencedTable, fk.ReferencedColumn, fk.ConstraintName))
				}
			}

			contextBuilder.WriteString("\n")
		}

		// Load and append views
		views, err := entry.source.GetViews(ctx)
		if err == nil && len(views) > 0 {
			contextBuilder.WriteString("#### Views\n\n")
			for _, v := range views {
				contextBuilder.WriteString(fmt.Sprintf("### View: %s\n", v.Name))
				if len(v.Columns) > 0 {
					contextBuilder.WriteString("Columns:\n")
					for _, col := range v.Columns {
						nullable := ""
						if col.IsNullable {
							nullable = " (nullable)"
						}
						contextBuilder.WriteString(fmt.Sprintf("  - %s: %s%s\n", col.Name, col.DataType, nullable))
					}
				}
				contextBuilder.WriteString("\n")
			}
		}

		// Load and append stored procedures
		procs, err := entry.source.GetProcedures(ctx)
		if err == nil && len(procs) > 0 {
			contextBuilder.WriteString("#### Stored Procedures\n\n")
			contextBuilder.WriteString("(Use the 'execute_procedure' tool to call these)\n\n")
			for _, p := range procs {
				contextBuilder.WriteString(fmt.Sprintf("### Stored Procedure: %s\n", p.Name))
				if p.Description != "" {
					contextBuilder.WriteString(fmt.Sprintf("Description: %s\n", p.Description))
				}
				if len(p.Parameters) > 0 {
					contextBuilder.WriteString("Parameters:\n")
					for _, param := range p.Parameters {
						contextBuilder.WriteString(fmt.Sprintf("  - %s (%s, %s)\n", param.Name, param.DataType, param.Mode))
					}
					// Build example usage hint
					exampleParams := make([]string, 0, len(p.Parameters))
					for _, param := range p.Parameters {
						cleanName := strings.TrimPrefix(param.Name, "@")
						exampleParams = append(exampleParams, fmt.Sprintf(`"%s":"<value>"`, cleanName))
					}
					contextBuilder.WriteString(fmt.Sprintf("Example: execute_procedure source_name='%s' procedure_name='%s' params='{%s}'\n",
						name, p.Name, strings.Join(exampleParams, ", ")))
				}
				contextBuilder.WriteString("\n")
			}
		}
	}

	contextBuilder.WriteString("\n=== END OF SCHEMA CONTEXT ===\n")
	contextBuilder.WriteString("\nUse the 'query_database' tool to execute SQL queries on these sources.\n")
	contextBuilder.WriteString("Use the 'list_views' tool to list views and 'list_procedures' to list stored procedures.\n")
	contextBuilder.WriteString("Use the 'execute_procedure' tool to run a stored procedure (non-read-only sources only).\n")

	ms.schemaContext = contextBuilder.String()
	return nil
}

// GetSchemaContext returns the pre-built schema context for use in AI prompts.
func (ms *MCPServer) GetSchemaContext() string {
	return ms.schemaContext
}

// AddCustomTool registers a custom tool with a predefined query.
// Each parameter carries a Type that determines the validation applied before
// the value is interpolated into the query template — see ToolParam for details.
func (ms *MCPServer) AddCustomTool(name, description, sourceName, query string, parameters []ToolParam) error {
	toolOptions := []mcp.ToolOption{
		mcp.WithDescription(description),
	}

	for _, param := range parameters {
		desc := param.Name
		if param.Type != "" && param.Type != "string" {
			desc = fmt.Sprintf("%s (%s)", param.Name, param.Type)
		}
		opt := mcp.WithString(param.Name, mcp.Description(desc))
		if param.Required {
			opt = mcp.WithString(param.Name, mcp.Required(), mcp.Description(desc))
		}
		toolOptions = append(toolOptions, opt)
	}

	tool := mcp.NewTool(name, toolOptions...)

	ms.customTools[name] = customToolEntry{
		sourceName: sourceName,
		query:      query,
		parameters: parameters,
	}

	// Capture tool name in closure so each tool routes to the correct entry.
	toolName := name
	ms.mcpServer.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return ms.handleNamedCustomTool(ctx, req, toolName)
	})

	return nil
}

func (ms *MCPServer) handleNamedCustomTool(ctx context.Context, request mcp.CallToolRequest, toolName string) (*mcp.CallToolResult, error) {
	toolEntry, exists := ms.customTools[toolName]
	if !exists {
		return mcp.NewToolResultError(fmt.Sprintf("Custom tool not found: %s", toolName)), nil
	}

	entry, exists := ms.sources[toolEntry.sourceName]
	if !exists {
		return mcp.NewToolResultError(fmt.Sprintf("Source not found: %s", toolEntry.sourceName)), nil
	}

	paramValues := make(map[string]string)
	for _, param := range toolEntry.parameters {
		raw := request.GetString(param.Name, param.Default)
		if raw == "" && param.Required {
			return mcp.NewToolResultError(fmt.Sprintf("Missing required parameter: %s", param.Name)), nil
		}
		coerced, err := validateAndCoerceParam(raw, param.Type)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Invalid parameter %q: %v", param.Name, err)), nil
		}
		paramValues[param.Name] = coerced
	}

	query := toolEntry.query
	for param, value := range paramValues {
		query = strings.ReplaceAll(query, fmt.Sprintf("{{%s}}", param), value)
	}

	// Apply the same validation and row-limiting pipeline as the standard query handler.
	if err := ms.queryValidator.ValidateQuery(query); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Query validation failed: %v", err)), nil
	}

	limitedQuery, err := ms.queryModifier.AddRowLimit(query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to add row limit: %v", err)), nil
	}

	result, err := entry.source.ExecuteQuery(ctx, limitedQuery)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Query execution failed: %v", err)), nil
	}

	// Apply PII masking to results.
	if ms.piiMasker != nil {
		for i := range result.Rows {
			for key, value := range result.Rows[i] {
				result.Rows[i][key] = ms.piiMasker.MaskValue(value)
			}
		}
	}

	jsonResult, err := json.MarshalIndent(result.Rows, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("Failed to marshal result"), nil
	}

	return mcp.NewToolResultText(string(jsonResult)), nil
}

// StartStdio starts the MCP server using stdio transport.
// This is the standard transport for Claude Desktop integration.
func (ms *MCPServer) StartStdio() error {
	return server.ServeStdio(ms.mcpServer)
}

func (ms *MCPServer) handleQueryDatabase(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sourceName, err := request.RequireString("source_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	entry, exists := ms.sources[sourceName]
	if !exists {
		return mcp.NewToolResultError(fmt.Sprintf("Source not found: %s", sourceName)), nil
	}

	// Enhanced query validation using AST parser
	if err := ms.queryValidator.ValidateQuery(query); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Query validation failed: %v", err)), nil
	}

	// Add row limit to prevent database overload
	modifiedQuery, err := ms.queryModifier.AddRowLimit(query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to add row limit: %v", err)), nil
	}

	// Execute query
	result, err := entry.source.ExecuteQuery(ctx, modifiedQuery)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Query execution failed: %v", err)), nil
	}

	// Apply PII masking to results
	if ms.piiMasker != nil {
		for i := range result.Rows {
			for key, value := range result.Rows[i] {
				result.Rows[i][key] = ms.piiMasker.MaskValue(value)
			}
		}
	}

	jsonResult, err := json.MarshalIndent(result.Rows, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("Failed to marshal result"), nil
	}

	return mcp.NewToolResultText(string(jsonResult)), nil
}

func (ms *MCPServer) handleListTables(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sourceName, err := request.RequireString("source_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	entry, exists := ms.sources[sourceName]
	if !exists {
		return mcp.NewToolResultError(fmt.Sprintf("Source not found: %s", sourceName)), nil
	}

	schemas, err := entry.source.GetSchema(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get schema: %v", err)), nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("# Tables in %s\n\n", sourceName))

	for _, table := range schemas {
		result.WriteString(fmt.Sprintf("## %s\n", table.Name))
		result.WriteString(fmt.Sprintf("- Columns: %d\n", len(table.Columns)))
		if len(table.PrimaryKeys) > 0 {
			result.WriteString(fmt.Sprintf("- Primary Keys: %s\n", strings.Join(table.PrimaryKeys, ", ")))
		}
		if len(table.ForeignKeys) > 0 {
			result.WriteString(fmt.Sprintf("- Foreign Keys: %d\n", len(table.ForeignKeys)))
		}
		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

func (ms *MCPServer) handleDescribeTable(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sourceName, err := request.RequireString("source_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	tableName, err := request.RequireString("table_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	entry, exists := ms.sources[sourceName]
	if !exists {
		return mcp.NewToolResultError(fmt.Sprintf("Source not found: %s", sourceName)), nil
	}

	// Use cached schemas when available to avoid reloading all tables on every call.
	schemas, cached := ms.tableSchemas[sourceName]
	if !cached {
		var err error
		schemas, err = entry.source.GetSchema(ctx)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to get schema: %v", err)), nil
		}
	}

	// Find the requested table
	var tableSchema *core.TableSchema
	for _, schema := range schemas {
		if strings.EqualFold(schema.Name, tableName) {
			tableSchema = &schema
			break
		}
	}

	if tableSchema == nil {
		return mcp.NewToolResultError(fmt.Sprintf("Table not found: %s", tableName)), nil
	}

	// Build detailed description
	var result strings.Builder
	result.WriteString(fmt.Sprintf("# Table: %s\n\n", tableSchema.Name))

	// Primary Keys
	if len(tableSchema.PrimaryKeys) > 0 {
		result.WriteString(fmt.Sprintf("**Primary Key(s):** %s\n\n", strings.Join(tableSchema.PrimaryKeys, ", ")))
	}

	// Columns
	result.WriteString("## Columns\n\n")
	result.WriteString("| Column | Type | Nullable | Description |\n")
	result.WriteString("|--------|------|----------|-------------|\n")

	for _, col := range tableSchema.Columns {
		nullable := "NO"
		if col.IsNullable {
			nullable = "YES"
		}
		desc := col.Description
		if desc == "" {
			desc = "-"
		}
		result.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", col.Name, col.DataType, nullable, desc))
	}
	result.WriteString("\n")

	// Foreign Keys
	if len(tableSchema.ForeignKeys) > 0 {
		result.WriteString("## Foreign Keys\n\n")
		for _, fk := range tableSchema.ForeignKeys {
			result.WriteString(fmt.Sprintf("- **%s** → %s.%s (%s)\n",
				fk.ColumnName, fk.ReferencedTable, fk.ReferencedColumn, fk.ConstraintName))
		}
		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

func (ms *MCPServer) handleListViews(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sourceName, err := request.RequireString("source_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	entry, exists := ms.sources[sourceName]
	if !exists {
		return mcp.NewToolResultError(fmt.Sprintf("Source not found: %s", sourceName)), nil
	}

	views, err := entry.source.GetViews(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list views: %v", err)), nil
	}

	if len(views) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("No views found in source '%s'.", sourceName)), nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("# Views in %s\n\n", sourceName))

	for _, v := range views {
		result.WriteString(fmt.Sprintf("## %s\n", v.Name))
		if len(v.Columns) > 0 {
			result.WriteString("| Column | Type | Nullable |\n")
			result.WriteString("|--------|------|----------|\n")
			for _, col := range v.Columns {
				nullable := "NO"
				if col.IsNullable {
					nullable = "YES"
				}
				result.WriteString(fmt.Sprintf("| %s | %s | %s |\n", col.Name, col.DataType, nullable))
			}
		}
		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

func (ms *MCPServer) handleListProcedures(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sourceName, err := request.RequireString("source_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	entry, exists := ms.sources[sourceName]
	if !exists {
		return mcp.NewToolResultError(fmt.Sprintf("Source not found: %s", sourceName)), nil
	}

	procs, err := entry.source.GetProcedures(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to list procedures: %v", err)), nil
	}

	if len(procs) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("No stored procedures found in source '%s'.", sourceName)), nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("# Stored Procedures in %s\n\n", sourceName))

	for _, p := range procs {
		result.WriteString(fmt.Sprintf("## %s\n", p.Name))
		if p.Description != "" {
			result.WriteString(fmt.Sprintf("*%s*\n\n", p.Description))
		}
		if len(p.Parameters) > 0 {
			result.WriteString("| Parameter | Type | Mode |\n")
			result.WriteString("|-----------|------|------|\n")
			for _, param := range p.Parameters {
				result.WriteString(fmt.Sprintf("| %s | %s | %s |\n", param.Name, param.DataType, param.Mode))
			}
			// Build example call hint
			exampleParams := make([]string, 0, len(p.Parameters))
			for _, param := range p.Parameters {
				cleanName := strings.TrimPrefix(param.Name, "@")
				exampleParams = append(exampleParams, fmt.Sprintf(`"%s":"<value>"`, cleanName))
			}
			result.WriteString(fmt.Sprintf("\n**Example call:** `execute_procedure source_name='%s' procedure_name='%s' params='{%s}'`\n",
				sourceName, p.Name, strings.Join(exampleParams, ", ")))
		} else {
			result.WriteString(fmt.Sprintf("\n**Example call:** `execute_procedure source_name='%s' procedure_name='%s'`\n", sourceName, p.Name))
		}
		result.WriteString("\n")
	}

	return mcp.NewToolResultText(result.String()), nil
}

func (ms *MCPServer) handleExecuteProcedure(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sourceName, err := request.RequireString("source_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	procName, err := request.RequireString("procedure_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	entry, exists := ms.sources[sourceName]
	if !exists {
		return mcp.NewToolResultError(fmt.Sprintf("Source not found: %s", sourceName)), nil
	}

	if entry.readOnly {
		return mcp.NewToolResultError(fmt.Sprintf("Source '%s' is read-only; stored procedure execution is not permitted", sourceName)), nil
	}

	// Validate procedure name to prevent SQL injection.
	if !safeIdentifierRe.MatchString(procName) {
		return mcp.NewToolResultError("Invalid procedure name: only alphanumeric characters, underscores, dots, and brackets are allowed"), nil
	}

	// Parse optional params JSON
	params := make(map[string]string)
	paramsJSON := request.GetString("params", "")
	if paramsJSON != "" {
		if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Invalid params JSON: %v", err)), nil
		}
	}

	result, err := entry.source.ExecuteProcedure(ctx, procName, params)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Procedure execution failed: %v", err)), nil
	}

	jsonResult, err := json.MarshalIndent(result.Rows, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("Failed to marshal result"), nil
	}

	return mcp.NewToolResultText(string(jsonResult)), nil
}

func (ms *MCPServer) handleSchemaPrompt(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	if ms.schemaContext == "" {
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{
					Role: mcp.RoleUser,
					Content: mcp.TextContent{
						Type: "text",
						Text: "No database schema information is available yet. Please ensure sources are connected and LoadSchemas() has been called.",
					},
				},
			},
		}, nil
	}

	return &mcp.GetPromptResult{
		Description: "Complete database schema with tables, columns, data types, primary keys, foreign keys, and column descriptions",
		Messages: []mcp.PromptMessage{
			{
				Role: mcp.RoleUser,
				Content: mcp.TextContent{
					Type: "text",
					Text: ms.schemaContext,
				},
			},
		},
	}, nil
}
