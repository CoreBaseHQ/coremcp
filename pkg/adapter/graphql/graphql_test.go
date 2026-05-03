package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// minimalIntrospectionResponse builds a GraphQL introspection response with the given query/mutation types.
func minimalIntrospectionResponse(queryTypeName, mutationTypeName string) map[string]interface{} {
	queryType := interface{}(nil)
	if queryTypeName != "" {
		queryType = map[string]interface{}{"name": queryTypeName}
	}
	mutationType := interface{}(nil)
	if mutationTypeName != "" {
		mutationType = map[string]interface{}{"name": mutationTypeName}
	}

	types := []map[string]interface{}{
		{
			"kind": "OBJECT", "name": queryTypeName, "description": "",
			"fields": []map[string]interface{}{
				{
					"name": "user", "description": "Get a user",
					"args": []map[string]interface{}{
						{
							"name": "id", "description": "User ID",
							"type":         map[string]interface{}{"kind": "NON_NULL", "name": nil, "ofType": map[string]interface{}{"kind": "SCALAR", "name": "ID", "ofType": nil}},
							"defaultValue": nil,
						},
					},
					"type":              map[string]interface{}{"kind": "OBJECT", "name": "User", "ofType": nil},
					"isDeprecated":      false,
					"deprecationReason": nil,
				},
			},
			"inputFields": nil,
		},
		{
			"kind": "OBJECT", "name": "User", "description": "A user object",
			"fields": []map[string]interface{}{
				{
					"name": "id", "description": "",
					"args":              []map[string]interface{}{},
					"type":              map[string]interface{}{"kind": "NON_NULL", "name": nil, "ofType": map[string]interface{}{"kind": "SCALAR", "name": "ID", "ofType": nil}},
					"isDeprecated":      false,
					"deprecationReason": nil,
				},
				{
					"name": "name", "description": "",
					"args":              []map[string]interface{}{},
					"type":              map[string]interface{}{"kind": "SCALAR", "name": "String", "ofType": nil},
					"isDeprecated":      false,
					"deprecationReason": nil,
				},
			},
			"inputFields": nil,
		},
		{
			"kind": "SCALAR", "name": "__Schema", "description": "introspection",
			"fields": nil, "inputFields": nil,
		},
	}

	if mutationTypeName != "" {
		types = append(types, map[string]interface{}{
			"kind": "OBJECT", "name": mutationTypeName, "description": "",
			"fields": []map[string]interface{}{
				{
					"name": "createUser", "description": "Create a user",
					"args": []map[string]interface{}{
						{
							"name": "name", "description": "User name",
							"type":         map[string]interface{}{"kind": "SCALAR", "name": "String", "ofType": nil},
							"defaultValue": nil,
						},
					},
					"type":              map[string]interface{}{"kind": "OBJECT", "name": "User", "ofType": nil},
					"isDeprecated":      false,
					"deprecationReason": nil,
				},
			},
			"inputFields": nil,
		})
	}

	schema := map[string]interface{}{
		"queryType":        queryType,
		"mutationType":     mutationType,
		"subscriptionType": nil,
		"types":            types,
		"directives":       []interface{}{},
	}

	return map[string]interface{}{
		"data": map[string]interface{}{
			"__schema": schema,
		},
	}
}

// buildGraphQLServer creates an httptest.Server that responds to introspection and regular queries.
func buildGraphQLServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)

		query, _ := req["query"].(string)
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(query, "__schema") {
			_ = json.NewEncoder(w).Encode(minimalIntrospectionResponse("Query", "Mutation"))
			return
		}

		// Regular query response
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{"id": "1", "name": "Alice"},
			},
		})
	}))
}

func TestNew_ValidDSN(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("graphql://%s/graphql", host)
	adapter, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if adapter == nil {
		t.Fatal("expected non-nil adapter")
	}
}

func TestNew_InvalidURL(t *testing.T) {
	_, err := New("://broken")
	if err == nil {
		t.Fatal("expected error for invalid DSN")
	}
}

func TestNew_WrongScheme(t *testing.T) {
	_, err := New("https://example.com/graphql")
	if err == nil {
		t.Fatal("expected error for wrong scheme")
	}
	if !strings.Contains(err.Error(), "invalid scheme") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNew_APIKeyBecomesAuthHeader(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("graphql://%s/graphql?apiKey=tok123", host)
	src, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ga := src.(*GraphQLAdapter)
	if ga.headers["Authorization"] != "Bearer tok123" {
		t.Errorf("expected 'Bearer tok123', got %q", ga.headers["Authorization"])
	}
}

func TestNew_CustomHeaders(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("graphql://%s/graphql?header_X-Org=acme", host)
	src, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ga := src.(*GraphQLAdapter)
	if ga.headers["X-Org"] != "acme" {
		t.Errorf("expected X-Org header 'acme', got %q", ga.headers["X-Org"])
	}
}

func TestNew_LocalhostUsesHTTP(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	port := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[1]
	dsn := fmt.Sprintf("graphql://localhost:%s/graphql", port)
	src, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ga := src.(*GraphQLAdapter)
	if !strings.HasPrefix(ga.endpoint, "http://") {
		t.Errorf("expected http:// endpoint for localhost, got %q", ga.endpoint)
	}
}

func TestNew_IntrospectsOnCreation(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))
	ga := src.(*GraphQLAdapter)

	if !ga.discovered {
		t.Error("expected adapter to have introspected schema")
	}
	if ga.schema == nil {
		t.Error("expected non-nil schema")
	}
}

func TestGraphQLAdapter_Name(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	if src.Name() == "" {
		t.Error("expected non-empty name")
	}
}

func TestGraphQLAdapter_Connect_AlreadyDiscovered(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	if err := src.Connect(context.Background()); err != nil {
		t.Errorf("Connect() unexpected error: %v", err)
	}
}

func TestGraphQLAdapter_Close(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	if err := src.Close(context.Background()); err != nil {
		t.Errorf("Close() unexpected error: %v", err)
	}
}

func TestGraphQLAdapter_GetViews(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	views, err := src.GetViews(context.Background())
	if err != nil {
		t.Fatalf("GetViews() error: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("expected no views, got %d", len(views))
	}
}

func TestGraphQLAdapter_GetSchema_NotDiscovered(t *testing.T) {
	ga := &GraphQLAdapter{name: "test", discovered: false}

	tables, err := ga.GetSchema(context.Background())
	if err != nil {
		t.Fatalf("GetSchema() error: %v", err)
	}
	if len(tables) != 0 {
		t.Errorf("expected empty schema when not discovered, got %d", len(tables))
	}
}

func TestGraphQLAdapter_GetSchema_SkipsInternalTypes(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	tables, err := src.GetSchema(context.Background())
	if err != nil {
		t.Fatalf("GetSchema() error: %v", err)
	}

	for _, tbl := range tables {
		if strings.HasPrefix(tbl.Name, "__") {
			t.Errorf("internal type %q should be excluded from schema", tbl.Name)
		}
	}
}

func TestGraphQLAdapter_GetSchema_ContainsUserType(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	tables, err := src.GetSchema(context.Background())
	if err != nil {
		t.Fatalf("GetSchema() error: %v", err)
	}

	found := false
	for _, tbl := range tables {
		if tbl.Name == "User" {
			found = true
			if len(tbl.Columns) == 0 {
				t.Error("User type should have columns")
			}
		}
	}
	if !found {
		t.Error("expected 'User' type in schema")
	}
}

func TestGraphQLAdapter_GetSchema_NonNullFieldNotNullable(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	tables, _ := src.GetSchema(context.Background())

	for _, tbl := range tables {
		if tbl.Name == "User" {
			for _, col := range tbl.Columns {
				if col.Name == "id" && col.IsNullable {
					t.Error("NON_NULL field 'id' should not be nullable")
				}
				if col.Name == "name" && !col.IsNullable {
					t.Error("nullable field 'name' should be nullable")
				}
			}
		}
	}
}

func TestGraphQLAdapter_GetProcedures_NotDiscovered(t *testing.T) {
	ga := &GraphQLAdapter{name: "test", discovered: false}

	procs, err := ga.GetProcedures(context.Background())
	if err != nil {
		t.Fatalf("GetProcedures() error: %v", err)
	}
	if len(procs) != 0 {
		t.Errorf("expected no procedures when not discovered, got %d", len(procs))
	}
}

func TestGraphQLAdapter_GetProcedures_NamesAreTypePrefixed(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	procs, err := src.GetProcedures(context.Background())
	if err != nil {
		t.Fatalf("GetProcedures() error: %v", err)
	}
	if len(procs) == 0 {
		t.Fatal("expected at least one procedure")
	}

	for _, p := range procs {
		if !strings.HasPrefix(p.Name, "query_") && !strings.HasPrefix(p.Name, "mutation_") {
			t.Errorf("procedure name %q should be prefixed with 'query_' or 'mutation_'", p.Name)
		}
	}
}

func TestGraphQLAdapter_GetOperations_NotDiscovered(t *testing.T) {
	ga := &GraphQLAdapter{name: "test", discovered: false}
	ops := ga.GetOperations()
	if len(ops) != 0 {
		t.Errorf("expected no operations when not discovered, got %d", len(ops))
	}
}

func TestGraphQLAdapter_GetOperations_QueryAndMutation(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))
	ga := src.(*GraphQLAdapter)

	ops := ga.GetOperations()
	if len(ops) == 0 {
		t.Fatal("expected operations")
	}

	queries := 0
	mutations := 0
	for _, op := range ops {
		switch op.Type {
		case "query":
			queries++
		case "mutation":
			mutations++
		}
	}

	if queries == 0 {
		t.Error("expected at least one query operation")
	}
	if mutations == 0 {
		t.Error("expected at least one mutation operation")
	}
}

func TestGraphQLAdapter_ExecuteQuery_RawGraphQL(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	result, err := src.ExecuteQuery(context.Background(), `query { user(id: "1") { name } }`)
	if err != nil {
		t.Fatalf("ExecuteQuery() error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Rows) == 0 {
		t.Error("expected rows in result")
	}
	if result.Columns[0] != "data" {
		t.Errorf("expected column 'data', got %q", result.Columns[0])
	}
}

func TestGraphQLAdapter_ExecuteQuery_WithVariables(t *testing.T) {
	var receivedVars map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if vars, ok := req["variables"].(map[string]interface{}); ok {
			receivedVars = vars
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{}})
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))
	ga := src.(*GraphQLAdapter)
	ga.discovered = true // skip introspection side-effect

	vars := map[string]interface{}{"id": "42"}
	_, err := ga.ExecuteQuery(context.Background(), `query($id: ID!) { user(id: $id) { name } }`, vars)
	if err != nil {
		t.Fatalf("ExecuteQuery() error: %v", err)
	}
	if receivedVars["id"] != "42" {
		t.Errorf("expected variable id=42, got %v", receivedVars)
	}
}

func TestGraphQLAdapter_ExecuteProcedure_InvalidFormat(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	_, err := src.ExecuteProcedure(context.Background(), "badformat", nil)
	if err == nil {
		t.Fatal("expected error for invalid operation name format")
	}
	if !strings.Contains(err.Error(), "invalid operation name format") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGraphQLAdapter_ExecuteProcedure_QueryNoParams(t *testing.T) {
	var receivedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		receivedQuery, _ = req["query"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"users": []interface{}{}},
		})
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	result, err := src.ExecuteProcedure(context.Background(), "query_users", nil)
	if err != nil {
		t.Fatalf("ExecuteProcedure() error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !strings.Contains(receivedQuery, "query") {
		t.Errorf("expected 'query' in built GraphQL query, got %q", receivedQuery)
	}
	if !strings.Contains(receivedQuery, "users") {
		t.Errorf("expected field name 'users' in query, got %q", receivedQuery)
	}
}

func TestGraphQLAdapter_ExecuteProcedure_MutationWithParams(t *testing.T) {
	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"createUser": map[string]interface{}{"id": "10"}},
		})
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))

	_, err := src.ExecuteProcedure(context.Background(), "mutation_createUser", map[string]string{
		"name": "Bob",
	})
	if err != nil {
		t.Fatalf("ExecuteProcedure() error: %v", err)
	}

	q, _ := receivedBody["query"].(string)
	if !strings.Contains(q, "mutation") {
		t.Errorf("expected 'mutation' keyword in query, got %q", q)
	}
	if !strings.Contains(q, "createUser") {
		t.Errorf("expected field 'createUser' in query, got %q", q)
	}

	vars, _ := receivedBody["variables"].(map[string]interface{})
	if vars["name"] != "Bob" {
		t.Errorf("expected variable name=Bob, got %v", vars)
	}
}

func TestGraphQLAdapter_ExecuteGraphQL_ReturnsGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{
				{"message": "field 'foo' not found"},
			},
		})
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))
	ga := src.(*GraphQLAdapter)
	ga.discovered = true

	_, err := ga.ExecuteQuery(context.Background(), "{ foo }")
	if err == nil {
		t.Fatal("expected error for GraphQL errors response")
	}
	if !strings.Contains(err.Error(), "GraphQL errors") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGraphQLAdapter_GetSchemaDescription_NotDiscovered(t *testing.T) {
	ga := &GraphQLAdapter{
		name:       "myapi",
		endpoint:   "http://myapi/graphql",
		discovered: false,
	}
	desc := ga.GetSchemaDescription()
	if !strings.Contains(desc, "not introspected") {
		t.Errorf("expected 'not introspected' in description, got %q", desc)
	}
}

func TestGraphQLAdapter_GetSchemaDescription_Discovered(t *testing.T) {
	srv := buildGraphQLServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))
	ga := src.(*GraphQLAdapter)

	desc := ga.GetSchemaDescription()
	if !strings.Contains(desc, "queries") || !strings.Contains(desc, "mutations") {
		t.Errorf("expected queries/mutations count in description, got %q", desc)
	}
}

func TestGraphQLAdapter_TypeName_Nil(t *testing.T) {
	ga := &GraphQLAdapter{}
	if ga.typeName(nil) != "Unknown" {
		t.Error("expected 'Unknown' for nil TypeDesc")
	}
}

func TestGraphQLAdapter_TypeName_Named(t *testing.T) {
	ga := &GraphQLAdapter{}
	td := &TypeDesc{Kind: "SCALAR", Name: "String"}
	if ga.typeName(td) != "String" {
		t.Errorf("expected 'String', got %q", ga.typeName(td))
	}
}

func TestGraphQLAdapter_TypeName_WrappedNonNull(t *testing.T) {
	ga := &GraphQLAdapter{}
	td := &TypeDesc{
		Kind:   "NON_NULL",
		OfType: &TypeDesc{Kind: "SCALAR", Name: "Int"},
	}
	if ga.typeName(td) != "Int" {
		t.Errorf("expected 'Int' for NON_NULL<Int>, got %q", ga.typeName(td))
	}
}

func TestGraphQLAdapter_TypeName_KindFallback(t *testing.T) {
	ga := &GraphQLAdapter{}
	td := &TypeDesc{Kind: "LIST"}
	if ga.typeName(td) != "LIST" {
		t.Errorf("expected kind 'LIST' as fallback, got %q", ga.typeName(td))
	}
}

func TestGraphQLAdapter_IsNonNull(t *testing.T) {
	ga := &GraphQLAdapter{}

	cases := []struct {
		name     string
		td       *TypeDesc
		expected bool
	}{
		{"nil", nil, false},
		{"scalar", &TypeDesc{Kind: "SCALAR", Name: "String"}, false},
		{"non-null", &TypeDesc{Kind: "NON_NULL"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ga.isNonNull(tc.td)
			if got != tc.expected {
				t.Errorf("isNonNull(%v) = %v, want %v", tc.td, got, tc.expected)
			}
		})
	}
}

func TestGraphQLAdapter_Introspect_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	// New() tolerates introspection failure, so check discovered = false
	src, err := New(fmt.Sprintf("graphql://%s/graphql", host))
	if err != nil {
		t.Fatalf("New() should not fail on bad introspection: %v", err)
	}
	ga := src.(*GraphQLAdapter)
	if ga.discovered {
		t.Error("adapter should not be discovered when introspection returns invalid JSON")
	}
}

func TestGraphQLAdapter_Introspect_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{{"message": "not authorized"}},
		})
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, err := New(fmt.Sprintf("graphql://%s/graphql", host))
	if err != nil {
		t.Fatalf("New() should not fail on introspection error: %v", err)
	}
	ga := src.(*GraphQLAdapter)
	if ga.discovered {
		t.Error("adapter should not be discovered when introspection returns errors")
	}
}

func TestGraphQLAdapter_FindType_NotFound(t *testing.T) {
	ga := &GraphQLAdapter{
		schema: &GraphQLSchema{},
	}
	ga.schema.Data.Schema = &IntrospectionSchema{
		Types: []FullType{{Name: "User"}},
	}

	if ga.findType("NonExistent") != nil {
		t.Error("expected nil for non-existent type")
	}
}

func TestGraphQLAdapter_FindType_Found(t *testing.T) {
	ga := &GraphQLAdapter{
		schema: &GraphQLSchema{},
	}
	ga.schema.Data.Schema = &IntrospectionSchema{
		Types: []FullType{{Name: "User", Kind: "OBJECT"}},
	}

	ft := ga.findType("User")
	if ft == nil {
		t.Fatal("expected to find 'User' type")
	}
	if ft.Name != "User" {
		t.Errorf("expected name 'User', got %q", ft.Name)
	}
}

func TestGraphQLAdapter_GetOperations_OnlyQueryType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(minimalIntrospectionResponse("Query", ""))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("graphql://%s/graphql", host))
	ga := src.(*GraphQLAdapter)

	ops := ga.GetOperations()
	for _, op := range ops {
		if op.Type == "mutation" {
			t.Errorf("unexpected mutation operation %q when no mutation type", op.Name)
		}
	}
}
