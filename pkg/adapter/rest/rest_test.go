package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildTestServer creates an httptest.Server that serves a minimal OpenAPI spec and handles endpoint calls.
func buildTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		spec := OpenAPISpec{
			OpenAPI: "3.0.0",
			Info: OpenAPIInfo{
				Title:       "Test API",
				Description: "A test REST API",
				Version:     "1.0",
			},
			Paths: map[string]OpenAPIPath{
				"/users": {
					"get": OpenAPIOperation{
						OperationID: "listUsers",
						Summary:     "List users",
						Description: "Returns all users",
						Parameters: []OpenAPIParameter{
							{Name: "limit", In: "query", Required: false, Schema: &struct {
								Type   string   `json:"type"`
								Enum   []string `json:"enum"`
								Format string   `json:"format"`
							}{Type: "integer"}},
						},
					},
				},
				"/users/{id}": {
					"get": OpenAPIOperation{
						OperationID: "getUser",
						Summary:     "Get user by ID",
						Parameters: []OpenAPIParameter{
							{Name: "id", In: "path", Required: true, Schema: &struct {
								Type   string   `json:"type"`
								Enum   []string `json:"enum"`
								Format string   `json:"format"`
							}{Type: "string"}},
						},
					},
					"post": OpenAPIOperation{
						OperationID: "updateUser",
						Summary:     "Update user",
						Parameters: []OpenAPIParameter{
							{Name: "id", In: "path", Required: true, Schema: &struct {
								Type   string   `json:"type"`
								Enum   []string `json:"enum"`
								Format string   `json:"format"`
							}{Type: "string"}},
						},
						RequestBody: &OpenAPIRequestBody{
							Content: map[string]struct {
								Schema json.RawMessage `json:"schema"`
							}{
								"application/json": {},
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(spec)
	})

	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "42", "name": "Alice"})
	})

	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": "1", "name": "Alice"},
			{"id": "2", "name": "Bob"},
		})
	})

	return httptest.NewServer(mux)
}

func TestNew_ValidDSN(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", strings.TrimPrefix(srv.URL, "http://"), srv.URL)
	adapter, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if adapter == nil {
		t.Fatal("expected non-nil adapter")
	}
}

func TestNew_InvalidURL(t *testing.T) {
	_, err := New("://broken-url")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestNew_WrongScheme(t *testing.T) {
	_, err := New("http://example.com")
	if err == nil {
		t.Fatal("expected error for wrong scheme")
	}
	if !strings.Contains(err.Error(), "invalid scheme") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestNew_APIKeyBecomesAuthHeader(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?apiKey=secret123", host)
	src, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ra := src.(*RESTAdapter)
	if ra.headers["Authorization"] != "Bearer secret123" {
		t.Errorf("expected Authorization header 'Bearer secret123', got %q", ra.headers["Authorization"])
	}
}

func TestNew_CustomHeaders(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?header_X-Tenant=acme&header_X-Version=2", host)
	src, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ra := src.(*RESTAdapter)
	if ra.headers["X-Tenant"] != "acme" {
		t.Errorf("expected X-Tenant header 'acme', got %q", ra.headers["X-Tenant"])
	}
	if ra.headers["X-Version"] != "2" {
		t.Errorf("expected X-Version header '2', got %q", ra.headers["X-Version"])
	}
}

func TestNew_LocalhostUsesHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	port := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[1]
	dsn := fmt.Sprintf("rest://localhost:%s/api", port)
	src, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ra := src.(*RESTAdapter)
	if !strings.HasPrefix(ra.baseURL, "http://") {
		t.Errorf("expected http:// baseURL for localhost, got %q", ra.baseURL)
	}
}

func TestNew_WithSpecURL(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, err := New(dsn)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ra := src.(*RESTAdapter)
	if !ra.discovered {
		t.Error("expected adapter to have discovered spec")
	}
	if ra.apiSpec == nil {
		t.Error("expected non-nil apiSpec")
	}
}

func TestRESTAdapter_Name_WithoutSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))
	ra := src.(*RESTAdapter)

	if ra.Name() == "" {
		t.Error("expected non-empty name")
	}
}

func TestRESTAdapter_Name_WithSpec(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, _ := New(dsn)

	if src.Name() != "Test API" {
		t.Errorf("expected name 'Test API', got %q", src.Name())
	}
}

func TestRESTAdapter_Connect(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))

	if err := src.Connect(context.Background()); err != nil {
		t.Errorf("Connect() unexpected error: %v", err)
	}
}

func TestRESTAdapter_Close(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))

	if err := src.Close(context.Background()); err != nil {
		t.Errorf("Close() unexpected error: %v", err)
	}
}

func TestRESTAdapter_GetViews(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))

	views, err := src.GetViews(context.Background())
	if err != nil {
		t.Fatalf("GetViews() error: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("expected empty views, got %d", len(views))
	}
}

func TestRESTAdapter_GetSchema_NoSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))

	tables, err := src.GetSchema(context.Background())
	if err != nil {
		t.Fatalf("GetSchema() error: %v", err)
	}
	if len(tables) != 0 {
		t.Errorf("expected empty schema without spec, got %d tables", len(tables))
	}
}

func TestRESTAdapter_GetSchema_WithSpec(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, _ := New(dsn)

	tables, err := src.GetSchema(context.Background())
	if err != nil {
		t.Fatalf("GetSchema() error: %v", err)
	}
	if len(tables) == 0 {
		t.Error("expected non-empty schema from spec")
	}

	// Every table should have a non-empty name
	for _, tbl := range tables {
		if tbl.Name == "" {
			t.Error("table name should not be empty")
		}
	}
}

func TestRESTAdapter_GetProcedures_NoSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))

	procs, err := src.GetProcedures(context.Background())
	if err != nil {
		t.Fatalf("GetProcedures() error: %v", err)
	}
	if len(procs) != 0 {
		t.Errorf("expected no procedures without spec, got %d", len(procs))
	}
}

func TestRESTAdapter_GetProcedures_WithSpec(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, _ := New(dsn)

	procs, err := src.GetProcedures(context.Background())
	if err != nil {
		t.Fatalf("GetProcedures() error: %v", err)
	}
	if len(procs) == 0 {
		t.Error("expected procedures from spec")
	}

	names := map[string]bool{}
	for _, p := range procs {
		names[p.Name] = true
	}
	for _, expected := range []string{"listUsers", "getUser", "updateUser"} {
		if !names[expected] {
			t.Errorf("expected procedure %q not found", expected)
		}
	}
}

func TestRESTAdapter_ExecuteQuery_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))

	_, err := src.ExecuteQuery(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("expected error from ExecuteQuery on REST adapter")
	}
}

func TestRESTAdapter_ExecuteProcedure_NotFound(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, _ := New(dsn)

	_, err := src.ExecuteProcedure(context.Background(), "nonExistentOp", nil)
	if err == nil {
		t.Fatal("expected error for non-existent endpoint")
	}
	if !strings.Contains(err.Error(), "endpoint not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRESTAdapter_ExecuteProcedure_MissingRequired(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, _ := New(dsn)

	// getUser requires "id" path param
	_, err := src.ExecuteProcedure(context.Background(), "getUser", map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing required parameter")
	}
	if !strings.Contains(err.Error(), "missing required parameter") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRESTAdapter_ExecuteProcedure_GETSuccess(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, _ := New(dsn)

	result, err := src.ExecuteProcedure(context.Background(), "getUser", map[string]string{"id": "42"})
	if err != nil {
		t.Fatalf("ExecuteProcedure() error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Rows) == 0 {
		t.Error("expected rows in result")
	}
}

func TestRESTAdapter_ExecuteProcedure_QueryParam(t *testing.T) {
	var receivedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]interface{}{})
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")

	// Manually build an adapter with a spec containing a query parameter
	src, _ := New(fmt.Sprintf("rest://%s", host))
	ra := src.(*RESTAdapter)
	ra.apiSpec = &OpenAPISpec{
		Info: OpenAPIInfo{Title: "T"},
		Paths: map[string]OpenAPIPath{
			"/items": {
				"get": OpenAPIOperation{
					OperationID: "listItems",
					Parameters: []OpenAPIParameter{
						{Name: "page", In: "query", Required: false, Schema: &struct {
							Type   string   `json:"type"`
							Enum   []string `json:"enum"`
							Format string   `json:"format"`
						}{Type: "integer"}},
					},
				},
			},
		},
	}
	ra.discovered = true

	_, err := ra.ExecuteProcedure(context.Background(), "listItems", map[string]string{"page": "3"})
	if err != nil {
		t.Fatalf("ExecuteProcedure() error: %v", err)
	}
	if !strings.Contains(receivedQuery, "page=3") {
		t.Errorf("expected query param 'page=3' in request, got %q", receivedQuery)
	}
}

func TestRESTAdapter_ExecuteProcedure_POSTWithBody(t *testing.T) {
	var receivedBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))
	ra := src.(*RESTAdapter)
	ra.apiSpec = &OpenAPISpec{
		Info: OpenAPIInfo{Title: "T"},
		Paths: map[string]OpenAPIPath{
			"/users/{id}": {
				"post": OpenAPIOperation{
					OperationID: "updateUser",
					Parameters: []OpenAPIParameter{
						{Name: "id", In: "path", Required: true, Schema: &struct {
							Type   string   `json:"type"`
							Enum   []string `json:"enum"`
							Format string   `json:"format"`
						}{Type: "string"}},
					},
					RequestBody: &OpenAPIRequestBody{
						Content: map[string]struct {
							Schema json.RawMessage `json:"schema"`
						}{"application/json": {}},
					},
				},
			},
		},
	}
	ra.discovered = true

	_, err := ra.ExecuteProcedure(context.Background(), "updateUser", map[string]string{
		"id":   "99",
		"name": "NewName",
	})
	if err != nil {
		t.Fatalf("ExecuteProcedure() error: %v", err)
	}
	if receivedBody["name"] != "NewName" {
		t.Errorf("expected body param 'name=NewName', got %v", receivedBody)
	}
}

func TestRESTAdapter_GetEndpoints_NoSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))
	ra := src.(*RESTAdapter)

	eps := ra.GetEndpoints()
	if len(eps) != 0 {
		t.Errorf("expected no endpoints without spec, got %d", len(eps))
	}
}

func TestRESTAdapter_GetEndpoints_WithSpec(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, _ := New(dsn)
	ra := src.(*RESTAdapter)

	eps := ra.GetEndpoints()
	if len(eps) == 0 {
		t.Error("expected endpoints from spec")
	}

	// Methods should be uppercase
	for _, ep := range eps {
		if ep.Method != strings.ToUpper(ep.Method) {
			t.Errorf("expected uppercase method, got %q", ep.Method)
		}
	}
}

func TestRESTAdapter_GetAPIDescription_NoSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))
	ra := src.(*RESTAdapter)

	desc := ra.GetAPIDescription()
	if desc == "" {
		t.Error("expected non-empty description")
	}
}

func TestRESTAdapter_GetAPIDescription_WithSpec(t *testing.T) {
	srv := buildTestServer(t)
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := fmt.Sprintf("rest://%s?specURL=%s/openapi.json", host, srv.URL)
	src, _ := New(dsn)
	ra := src.(*RESTAdapter)

	desc := ra.GetAPIDescription()
	if !strings.Contains(desc, "Test API") && !strings.Contains(desc, "A test REST API") {
		t.Errorf("expected spec title/description in output, got %q", desc)
	}
	if !strings.Contains(desc, "1.0") {
		t.Errorf("expected version in description, got %q", desc)
	}
}

func TestRESTAdapter_DiscoverFromSpec_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))
	ra := src.(*RESTAdapter)

	err := ra.discoverFromSpec(context.Background(), srv.URL+"/spec.json")
	if err == nil {
		t.Fatal("expected error for non-200 spec response")
	}
}

func TestRESTAdapter_DiscoverFromSpec_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))
	ra := src.(*RESTAdapter)

	err := ra.discoverFromSpec(context.Background(), srv.URL+"/spec.json")
	if err == nil {
		t.Fatal("expected error for invalid JSON spec")
	}
}

func TestRESTAdapter_GetSchema_ColumnsFromParameters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	src, _ := New(fmt.Sprintf("rest://%s", host))
	ra := src.(*RESTAdapter)

	ra.apiSpec = &OpenAPISpec{
		Info: OpenAPIInfo{Title: "T"},
		Paths: map[string]OpenAPIPath{
			"/search": {
				"get": OpenAPIOperation{
					OperationID: "search",
					Parameters: []OpenAPIParameter{
						{Name: "q", In: "query", Required: true, Schema: &struct {
							Type   string   `json:"type"`
							Enum   []string `json:"enum"`
							Format string   `json:"format"`
						}{Type: "string"}},
						{Name: "limit", In: "query", Required: false, Schema: &struct {
							Type   string   `json:"type"`
							Enum   []string `json:"enum"`
							Format string   `json:"format"`
						}{Type: "integer"}},
					},
				},
			},
		},
	}
	ra.discovered = true

	tables, err := ra.GetSchema(context.Background())
	if err != nil {
		t.Fatalf("GetSchema() error: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("expected at least one table")
	}

	tbl := tables[0]
	if len(tbl.Columns) != 2 {
		t.Errorf("expected 2 columns, got %d", len(tbl.Columns))
	}

	// Required param → IsNullable = false
	for _, col := range tbl.Columns {
		if col.Name == "q" && col.IsNullable {
			t.Error("required param 'q' should not be nullable")
		}
		if col.Name == "limit" && !col.IsNullable {
			t.Error("optional param 'limit' should be nullable")
		}
	}
}
