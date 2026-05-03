// Package graphql provides a GraphQL API adapter for CoreMCP.
// It can discover the schema via introspection and expose queries/mutations as MCP tools.
package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/corebasehq/coremcp/pkg/core"
)

// maxBodyBytes caps how much of a response body is read into memory (10 MB).
const maxBodyBytes = 10 * 1024 * 1024

// GraphQLAdapter provides access to GraphQL APIs with schema introspection.
type GraphQLAdapter struct {
	name       string
	endpoint   string
	headers    map[string]string
	client     *http.Client
	schema     *GraphQLSchema
	discovered bool
}

// GraphQLSchema represents the introspected GraphQL schema.
type GraphQLSchema struct {
	Data struct {
		Schema *IntrospectionSchema `json:"__schema"`
	} `json:"data"`
}

// IntrospectionSchema represents the full GraphQL schema.
type IntrospectionSchema struct {
	QueryType        *TypeRef                 `json:"queryType"`
	MutationType     *TypeRef                 `json:"mutationType"`
	SubscriptionType *TypeRef                 `json:"subscriptionType"`
	Types            []FullType               `json:"types"`
	Directives       []map[string]interface{} `json:"directives"`
}

// TypeRef is a reference to a GraphQL type.
type TypeRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// FullType represents a complete GraphQL type definition.
type FullType struct {
	Kind        string       `json:"kind"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Fields      []Field      `json:"fields"`
	InputFields []InputValue `json:"inputFields"`
}

// Field represents a field in a GraphQL type.
type Field struct {
	Name              string       `json:"name"`
	Description       string       `json:"description"`
	Args              []InputValue `json:"args"`
	Type              *TypeDesc    `json:"type"`
	IsDeprecated      bool         `json:"isDeprecated"`
	DeprecationReason string       `json:"deprecationReason"`
}

// InputValue represents an input value (argument or input field).
type InputValue struct {
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Type         *TypeDesc `json:"type"`
	DefaultValue *string   `json:"defaultValue"`
}

// TypeDesc describes a GraphQL type (possibly wrapped in NON_NULL or LIST).
type TypeDesc struct {
	Kind   string    `json:"kind"`
	Name   string    `json:"name"`
	OfType *TypeDesc `json:"ofType"`
}

// GraphQLOperation represents a discovered operation that can be exposed as an MCP tool.
type GraphQLOperation struct {
	Name        string
	Type        string // query, mutation, subscription
	Description string
	Fields      []Field
	ReturnType  string
}

// introspectionQuery is the standard GraphQL introspection query.
// ofType is nested 6 levels deep to correctly represent types like [[User!]!]!
// (NON_NULL → LIST → NON_NULL → LIST → NON_NULL → NamedType).
const introspectionQuery = `
query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types {
      kind
      name
      description
      fields(includeDeprecated: true) {
        name
        description
        args {
          name
          description
          type { name kind ofType { name kind ofType { name kind ofType { name kind ofType { name kind ofType { name kind } } } } } }
          defaultValue
        }
        type { name kind ofType { name kind ofType { name kind ofType { name kind ofType { name kind ofType { name kind } } } } } }
        isDeprecated
        deprecationReason
      }
      inputFields {
        name
        description
        type { name kind ofType { name kind ofType { name kind ofType { name kind ofType { name kind ofType { name kind } } } } } }
        defaultValue
      }
    }
  }
}
`

// New creates a new GraphQL API adapter.
// The dsn format is: graphql://endpoint?apiKey=xxx&timeout=30
// Headers can be provided as: header_X-Custom=value
func New(dsn string) (core.Source, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid GraphQL DSN: %w", err)
	}

	if u.Scheme != "graphql" {
		return nil, fmt.Errorf("invalid scheme %q, expected 'graphql'", u.Scheme)
	}

	endpoint := fmt.Sprintf("https://%s%s", u.Host, u.Path)
	if u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" {
		endpoint = fmt.Sprintf("http://%s%s", u.Host, u.Path)
	}

	headers := make(map[string]string)
	query := u.Query()

	if apiKey := query.Get("apiKey"); apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}

	for key, values := range query {
		if strings.HasPrefix(key, "header_") {
			headerName := strings.TrimPrefix(key, "header_")
			if headerName != "" {
				headers[headerName] = values[0]
			}
		}
	}

	// Parse optional timeout in seconds; default 30 s.
	timeout := 30 * time.Second
	if t := query.Get("timeout"); t != "" {
		if secs, err := strconv.Atoi(t); err == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}

	adapter := &GraphQLAdapter{
		name:     u.Hostname(),
		endpoint: strings.TrimSuffix(endpoint, "/"),
		headers:  headers,
		client:   &http.Client{Timeout: timeout},
	}

	if err := adapter.introspect(context.Background()); err != nil {
		slog.Warn("could not introspect GraphQL schema", "endpoint", adapter.endpoint, "error", err)
	}

	return adapter, nil
}

func (g *GraphQLAdapter) Name() string {
	return g.name
}

func (g *GraphQLAdapter) Connect(ctx context.Context) error {
	if !g.discovered {
		if err := g.introspect(ctx); err != nil {
			slog.Warn("GraphQL connection test failed (continuing)", "endpoint", g.endpoint, "error", err)
		}
	}
	return nil
}

func (g *GraphQLAdapter) Close(_ context.Context) error {
	return nil
}

// GetSchema returns the discovered GraphQL types as "tables" for schema context.
func (g *GraphQLAdapter) GetSchema(_ context.Context) ([]core.TableSchema, error) {
	if !g.discovered || g.schema == nil {
		return []core.TableSchema{}, nil
	}

	var tables []core.TableSchema
	for _, t := range g.schema.Data.Schema.Types {
		if strings.HasPrefix(t.Name, "__") {
			continue
		}
		var columns []core.ColumnInfo
		for _, field := range t.Fields {
			columns = append(columns, core.ColumnInfo{
				Name:        field.Name,
				DataType:    g.typeName(field.Type),
				IsNullable:  !g.isNonNull(field.Type),
				Description: field.Description,
			})
		}
		tables = append(tables, core.TableSchema{
			Name:        t.Name,
			Columns:     columns,
			PrimaryKeys: []string{},
			ForeignKeys: []core.ForeignKey{},
		})
	}

	return tables, nil
}

func (g *GraphQLAdapter) GetViews(_ context.Context) ([]core.ViewSchema, error) {
	return []core.ViewSchema{}, nil
}

// GetProcedures returns available queries and mutations as procedures.
func (g *GraphQLAdapter) GetProcedures(_ context.Context) ([]core.StoredProcedure, error) {
	ops := g.GetOperations()
	var procs []core.StoredProcedure
	for _, op := range ops {
		var params []core.ProcParameter
		for _, field := range op.Fields {
			for _, arg := range field.Args {
				params = append(params, core.ProcParameter{
					Name:     arg.Name,
					DataType: g.typeName(arg.Type),
					Mode:     "IN",
				})
			}
		}
		procs = append(procs, core.StoredProcedure{
			Name:        fmt.Sprintf("%s_%s", op.Type, op.Name),
			Description: op.Description,
			Parameters:  params,
		})
	}
	return procs, nil
}

// ExecuteQuery runs a raw GraphQL query string.
func (g *GraphQLAdapter) ExecuteQuery(ctx context.Context, query string, args ...any) (*core.QueryResult, error) {
	variables := make(map[string]interface{})
	if len(args) > 0 {
		if vmap, ok := args[0].(map[string]interface{}); ok {
			variables = vmap
		}
	}
	return g.executeGraphQL(ctx, query, variables)
}

// ExecuteProcedure executes a GraphQL operation by name.
// Variable types are resolved from the introspected schema; falls back to String.
func (g *GraphQLAdapter) ExecuteProcedure(ctx context.Context, name string, params map[string]string) (*core.QueryResult, error) {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid operation name format: %s (expected: type_fieldName)", name)
	}

	opType := parts[0]
	fieldName := parts[1]

	var argsBuilder strings.Builder
	var varsBuilder strings.Builder
	variables := make(map[string]interface{})

	if len(params) > 0 {
		argsBuilder.WriteString("(")
		varsBuilder.WriteString("(")

		first := true
		for k, v := range params {
			if !first {
				argsBuilder.WriteString(", ")
				varsBuilder.WriteString(", ")
			}
			first = false
			argTypeSig := g.argType(opType, fieldName, k)
			argsBuilder.WriteString(fmt.Sprintf("%s: $%s", k, k))
			varsBuilder.WriteString(fmt.Sprintf("$%s: %s", k, argTypeSig))
			variables[k] = v
		}

		argsBuilder.WriteString(")")
		varsBuilder.WriteString(")")
	}

	query := fmt.Sprintf(`%s %s {
  %s%s
}`, opType, varsBuilder.String(), fieldName, argsBuilder.String())

	return g.executeGraphQL(ctx, query, variables)
}

// introspect performs GraphQL schema introspection using the provided context.
func (g *GraphQLAdapter) introspect(ctx context.Context) error {
	result, err := g.executeGraphQL(ctx, introspectionQuery, nil)
	if err != nil {
		return fmt.Errorf("introspection query failed: %w", err)
	}

	if len(result.Rows) == 0 {
		return fmt.Errorf("introspection returned no data")
	}

	// executeGraphQL strips the outer "data" wrapper before returning.
	// Re-wrap so that GraphQLSchema (which expects {"data":{"__schema":{...}}}) unmarshals correctly.
	wrappedJSON, err := json.Marshal(map[string]interface{}{"data": result.Rows[0]["data"]})
	if err != nil {
		return fmt.Errorf("failed to marshal introspection data: %w", err)
	}

	var schema GraphQLSchema
	if err := json.Unmarshal(wrappedJSON, &schema); err != nil {
		return fmt.Errorf("failed to parse introspection result: %w", err)
	}

	if schema.Data.Schema == nil {
		return fmt.Errorf("introspection returned empty schema")
	}

	g.schema = &schema
	g.discovered = true

	queryType := "none"
	if schema.Data.Schema.QueryType != nil {
		queryType = schema.Data.Schema.QueryType.Name
	}
	mutationType := "none"
	if schema.Data.Schema.MutationType != nil {
		mutationType = schema.Data.Schema.MutationType.Name
	}
	slog.Info("GraphQL schema introspected",
		"types", len(schema.Data.Schema.Types),
		"queryType", queryType,
		"mutationType", mutationType)

	return nil
}

// GetOperations returns all available queries and mutations.
func (g *GraphQLAdapter) GetOperations() []GraphQLOperation {
	if !g.discovered || g.schema == nil {
		return []GraphQLOperation{}
	}

	var ops []GraphQLOperation

	if g.schema.Data.Schema.QueryType != nil {
		queryType := g.findType(g.schema.Data.Schema.QueryType.Name)
		if queryType != nil {
			for _, field := range queryType.Fields {
				ops = append(ops, GraphQLOperation{
					Name:        field.Name,
					Type:        "query",
					Description: field.Description,
					Fields:      []Field{field},
					ReturnType:  g.typeName(field.Type),
				})
			}
		}
	}

	if g.schema.Data.Schema.MutationType != nil {
		mutationType := g.findType(g.schema.Data.Schema.MutationType.Name)
		if mutationType != nil {
			for _, field := range mutationType.Fields {
				ops = append(ops, GraphQLOperation{
					Name:        field.Name,
					Type:        "mutation",
					Description: field.Description,
					Fields:      []Field{field},
					ReturnType:  g.typeName(field.Type),
				})
			}
		}
	}

	return ops
}

// findType locates a type by name in the schema.
func (g *GraphQLAdapter) findType(name string) *FullType {
	if g.schema == nil {
		return nil
	}
	for i := range g.schema.Data.Schema.Types {
		if g.schema.Data.Schema.Types[i].Name == name {
			return &g.schema.Data.Schema.Types[i]
		}
	}
	return nil
}

// typeName returns the base named type, unwrapping any NON_NULL/LIST wrappers.
func (g *GraphQLAdapter) typeName(t *TypeDesc) string {
	if t == nil {
		return "Unknown"
	}
	if t.Name != "" {
		return t.Name
	}
	if t.OfType != nil {
		return g.typeName(t.OfType)
	}
	return t.Kind
}

// typeSignature reconstructs the full GraphQL type string, e.g. "[String!]!".
func (g *GraphQLAdapter) typeSignature(t *TypeDesc) string {
	if t == nil {
		return "String"
	}
	switch t.Kind {
	case "NON_NULL":
		return g.typeSignature(t.OfType) + "!"
	case "LIST":
		return "[" + g.typeSignature(t.OfType) + "]"
	default:
		if t.Name != "" {
			return t.Name
		}
		return "String"
	}
}

// argType returns the full GraphQL type signature for a named argument on a field.
// Falls back to "String" when the schema is unavailable or the argument is not found.
func (g *GraphQLAdapter) argType(opType, fieldName, argName string) string {
	if g.schema == nil || g.schema.Data.Schema == nil {
		return "String"
	}

	var rootTypeName string
	switch opType {
	case "query":
		if g.schema.Data.Schema.QueryType != nil {
			rootTypeName = g.schema.Data.Schema.QueryType.Name
		}
	case "mutation":
		if g.schema.Data.Schema.MutationType != nil {
			rootTypeName = g.schema.Data.Schema.MutationType.Name
		}
	case "subscription":
		if g.schema.Data.Schema.SubscriptionType != nil {
			rootTypeName = g.schema.Data.Schema.SubscriptionType.Name
		}
	}

	if rootTypeName == "" {
		return "String"
	}

	t := g.findType(rootTypeName)
	if t == nil {
		return "String"
	}

	for _, field := range t.Fields {
		if field.Name == fieldName {
			for _, arg := range field.Args {
				if arg.Name == argName {
					return g.typeSignature(arg.Type)
				}
			}
		}
	}

	return "String"
}

// isNonNull reports whether a type is non-null.
func (g *GraphQLAdapter) isNonNull(t *TypeDesc) bool {
	if t == nil {
		return false
	}
	return t.Kind == "NON_NULL"
}

// executeGraphQL makes a GraphQL POST request and returns the data field.
func (g *GraphQLAdapter) executeGraphQL(ctx context.Context, query string, variables map[string]interface{}) (*core.QueryResult, error) {
	reqBody := map[string]interface{}{"query": query}
	if len(variables) > 0 {
		reqBody["variables"] = variables
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", g.endpoint, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range g.headers {
		req.Header.Set(k, v)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GraphQL request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GraphQL endpoint returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Treat any GraphQL errors as failure, including partial-success responses
	// (where both "data" and "errors" are present). Callers can retry with
	// adjusted queries.
	if errs, ok := result["errors"].([]interface{}); ok && len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			if emap, ok := e.(map[string]interface{}); ok {
				msgs = append(msgs, fmt.Sprintf("%v", emap["message"]))
			}
		}
		return nil, fmt.Errorf("GraphQL errors: %s", strings.Join(msgs, "; "))
	}

	return &core.QueryResult{
		Columns: []string{"data"},
		Rows:    []map[string]interface{}{{"data": result["data"]}},
	}, nil
}

// GetSchemaDescription returns a human-readable description of the schema.
func (g *GraphQLAdapter) GetSchemaDescription() string {
	if !g.discovered {
		return fmt.Sprintf("GraphQL API at %s (schema not introspected)", g.endpoint)
	}
	ops := g.GetOperations()
	queries, mutations := 0, 0
	for _, op := range ops {
		switch op.Type {
		case "query":
			queries++
		case "mutation":
			mutations++
		}
	}
	return fmt.Sprintf("GraphQL API at %s (%d queries, %d mutations)", g.endpoint, queries, mutations)
}
