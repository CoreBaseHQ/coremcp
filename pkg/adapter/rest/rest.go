// Package rest provides a REST API adapter for CoreMCP.
// It can discover API endpoints from OpenAPI/Swagger specs and expose them as MCP tools.
package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/corebasehq/coremcp/pkg/core"
)

// maxBodyBytes caps how much of a response or spec body is read into memory (10 MB).
const maxBodyBytes = 10 * 1024 * 1024

// RESTAdapter provides access to REST APIs with optional OpenAPI discovery.
type RESTAdapter struct {
	name       string
	baseURL    string
	headers    map[string]string
	client     *http.Client
	apiSpec    *OpenAPISpec
	discovered bool
}

// OpenAPISpec represents a minimal OpenAPI 3.0 structure for endpoint discovery.
type OpenAPISpec struct {
	OpenAPI string                 `json:"openapi"`
	Info    OpenAPIInfo            `json:"info"`
	Paths   map[string]OpenAPIPath `json:"paths"`
}

// OpenAPIInfo contains API metadata.
type OpenAPIInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

// OpenAPIPath represents the operations available on a path.
type OpenAPIPath map[string]OpenAPIOperation

// OpenAPIOperation represents a single endpoint operation.
type OpenAPIOperation struct {
	Summary     string              `json:"summary"`
	Description string              `json:"description"`
	OperationID string              `json:"operationId"`
	Parameters  []OpenAPIParameter  `json:"parameters"`
	RequestBody *OpenAPIRequestBody `json:"requestBody"`
}

// OpenAPIParameter represents a path, query, or header parameter.
type OpenAPIParameter struct {
	Name        string `json:"name"`
	In          string `json:"in"` // query, path, header
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Schema      *struct {
		Type   string   `json:"type"`
		Enum   []string `json:"enum"`
		Format string   `json:"format"`
	} `json:"schema"`
}

// OpenAPIRequestBody represents the request body structure.
type OpenAPIRequestBody struct {
	Content map[string]struct {
		Schema json.RawMessage `json:"schema"`
	} `json:"content"`
}

// Endpoint represents a discovered REST endpoint that can be exposed as an MCP tool.
type Endpoint struct {
	Path        string
	Method      string
	OperationID string
	Summary     string
	Description string
	Parameters  []EndpointParam
	HasBody     bool
}

// EndpointParam represents a parameter for an endpoint.
type EndpointParam struct {
	Name        string
	In          string // query, path, header
	Description string
	Required    bool
	Type        string
	Enum        []string
}

// New creates a new REST API adapter.
// The dsn format is: rest://baseURL?apiKey=xxx&specURL=xxx&timeout=30
// Headers can be provided as: header_X-Custom=value
func New(dsn string) (core.Source, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid REST DSN: %w", err)
	}

	if u.Scheme != "rest" {
		return nil, fmt.Errorf("invalid scheme %q, expected 'rest'", u.Scheme)
	}

	// Use http for localhost, https for all other hosts.
	scheme := "https"
	if u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s%s", scheme, u.Host, u.Path)

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

	adapter := &RESTAdapter{
		name:    u.Hostname(),
		baseURL: strings.TrimSuffix(baseURL, "/"),
		headers: headers,
		client:  &http.Client{Timeout: timeout},
	}

	if specURL := query.Get("specURL"); specURL != "" {
		if err := adapter.discoverFromSpec(context.Background(), specURL); err != nil {
			slog.Warn("could not load OpenAPI spec", "url", specURL, "error", err)
		}
	}

	return adapter, nil
}

func (r *RESTAdapter) Name() string {
	if r.apiSpec != nil && r.apiSpec.Info.Title != "" {
		return r.apiSpec.Info.Title
	}
	return r.name
}

func (r *RESTAdapter) Connect(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", r.baseURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create test request: %w", err)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		// Don't fail — the API might only respond to authenticated endpoints.
		slog.Warn("REST connection test failed (continuing)", "url", r.baseURL, "error", err)
		return nil
	}
	defer resp.Body.Close()
	slog.Info("REST adapter connected", "url", r.baseURL, "status", resp.StatusCode)
	return nil
}

func (r *RESTAdapter) Close(_ context.Context) error {
	return nil
}

// GetSchema returns the discovered API endpoints as "tables" for schema context.
// Each column's description includes the parameter location (path/query/header)
// so the AI can reconstruct the correct call.
func (r *RESTAdapter) GetSchema(ctx context.Context) ([]core.TableSchema, error) {
	endpoints := r.GetEndpoints()
	var tables []core.TableSchema

	for _, ep := range endpoints {
		tableName := fmt.Sprintf("%s_%s", ep.Method,
			strings.ReplaceAll(strings.Trim(ep.Path, "/"), "/", "_"))

		var columns []core.ColumnInfo
		for _, param := range ep.Parameters {
			desc := param.Description
			if desc != "" {
				desc = fmt.Sprintf("[%s] %s", param.In, desc)
			} else {
				desc = fmt.Sprintf("[%s] %s parameter", param.In, param.Name)
			}
			columns = append(columns, core.ColumnInfo{
				Name:        param.Name,
				DataType:    param.Type,
				IsNullable:  !param.Required,
				Description: desc,
			})
		}

		tables = append(tables, core.TableSchema{
			Name:        tableName,
			Columns:     columns,
			PrimaryKeys: []string{},
			ForeignKeys: []core.ForeignKey{},
		})
	}

	return tables, nil
}

func (r *RESTAdapter) GetViews(_ context.Context) ([]core.ViewSchema, error) {
	return []core.ViewSchema{}, nil
}

func (r *RESTAdapter) GetProcedures(_ context.Context) ([]core.StoredProcedure, error) {
	endpoints := r.GetEndpoints()
	var procs []core.StoredProcedure
	for _, ep := range endpoints {
		var params []core.ProcParameter
		for _, param := range ep.Parameters {
			params = append(params, core.ProcParameter{
				Name:     param.Name,
				DataType: param.Type,
				Mode:     "IN",
			})
		}
		procs = append(procs, core.StoredProcedure{
			Name:        ep.OperationID,
			Description: ep.Description,
			Parameters:  params,
		})
	}
	return procs, nil
}

// ExecuteQuery is not supported for REST; use ExecuteProcedure instead.
func (r *RESTAdapter) ExecuteQuery(_ context.Context, _ string, _ ...any) (*core.QueryResult, error) {
	return nil, fmt.Errorf("REST adapter does not support SQL queries; use API calls instead")
}

// ExecuteProcedure executes a REST API call by operation ID.
func (r *RESTAdapter) ExecuteProcedure(ctx context.Context, name string, params map[string]string) (*core.QueryResult, error) {
	endpoint := r.findEndpoint(name)
	if endpoint == nil {
		return nil, fmt.Errorf("endpoint not found: %s", name)
	}
	return r.callEndpoint(ctx, endpoint, params)
}

// discoverFromSpec loads and parses an OpenAPI spec from specURL.
// The Authorization header is intentionally NOT forwarded — spec endpoints are
// typically public and forwarding credentials to an arbitrary URL leaks the API key.
func (r *RESTAdapter) discoverFromSpec(ctx context.Context, specURL string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", specURL, nil)
	if err != nil {
		return err
	}

	for k, v := range r.headers {
		if strings.ToLower(k) != "authorization" {
			req.Header.Set(k, v)
		}
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("spec URL returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}

	var spec OpenAPISpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return fmt.Errorf("failed to parse OpenAPI spec: %w", err)
	}

	r.apiSpec = &spec
	r.discovered = true
	slog.Info("OpenAPI spec loaded", "paths", len(spec.Paths), "title", spec.Info.Title)
	return nil
}

// GetEndpoints returns all discovered endpoints in a stable sorted order.
// Endpoints without an operationId are skipped — they cannot be addressed by name.
func (r *RESTAdapter) GetEndpoints() []Endpoint {
	if !r.discovered || r.apiSpec == nil {
		return []Endpoint{}
	}

	var endpoints []Endpoint
	for path, pathItem := range r.apiSpec.Paths {
		for method, op := range pathItem {
			if op.OperationID == "" {
				continue
			}
			ep := Endpoint{
				Path:        path,
				Method:      strings.ToUpper(method),
				OperationID: op.OperationID,
				Summary:     op.Summary,
				Description: op.Description,
				HasBody:     op.RequestBody != nil,
			}
			for _, p := range op.Parameters {
				paramType := "string"
				if p.Schema != nil && p.Schema.Type != "" {
					paramType = p.Schema.Type
				}
				ep.Parameters = append(ep.Parameters, EndpointParam{
					Name:        p.Name,
					In:          p.In,
					Description: p.Description,
					Required:    p.Required,
					Type:        paramType,
				})
			}
			endpoints = append(endpoints, ep)
		}
	}

	// Sort for deterministic output across calls (map iteration is non-deterministic).
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Path != endpoints[j].Path {
			return endpoints[i].Path < endpoints[j].Path
		}
		return endpoints[i].Method < endpoints[j].Method
	})

	return endpoints
}

// findEndpoint locates an endpoint by operation ID.
func (r *RESTAdapter) findEndpoint(operationID string) *Endpoint {
	for _, ep := range r.GetEndpoints() {
		if ep.OperationID == operationID {
			return &ep
		}
	}
	return nil
}

// callEndpoint makes the actual HTTP request for an endpoint.
func (r *RESTAdapter) callEndpoint(ctx context.Context, ep *Endpoint, params map[string]string) (*core.QueryResult, error) {
	urlPath := ep.Path
	queryParams := url.Values{}

	for _, param := range ep.Parameters {
		value, ok := params[param.Name]
		if !ok {
			if param.Required {
				return nil, fmt.Errorf("missing required parameter: %s", param.Name)
			}
			continue
		}
		switch param.In {
		case "path":
			urlPath = strings.ReplaceAll(urlPath, "{"+param.Name+"}", url.PathEscape(value))
		case "query":
			queryParams.Set(param.Name, value)
		}
	}

	fullURL := r.baseURL + urlPath
	if len(queryParams) > 0 {
		fullURL += "?" + queryParams.Encode()
	}

	method := ep.Method
	if method == "" {
		method = "GET"
	}

	var body io.Reader
	if ep.HasBody {
		bodyData := make(map[string]string)
		for k, v := range params {
			isPathOrQuery := false
			for _, p := range ep.Parameters {
				if p.Name == k && (p.In == "path" || p.In == "query") {
					isPathOrQuery = true
					break
				}
			}
			if !isPathOrQuery {
				bodyData[k] = v
			}
		}
		if len(bodyData) > 0 {
			jsonBody, err := json.Marshal(bodyData)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal request body: %w", err)
			}
			body = strings.NewReader(string(jsonBody))
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var data interface{}
	if err := json.Unmarshal(respBody, &data); err != nil {
		data = string(respBody)
	}

	return &core.QueryResult{
		Columns: []string{"status", "data"},
		Rows: []map[string]interface{}{
			{"status": resp.StatusCode, "data": data},
		},
	}, nil
}

// GetAPIDescription returns a description of the API for AI context.
func (r *RESTAdapter) GetAPIDescription() string {
	if r.apiSpec == nil {
		return fmt.Sprintf("REST API at %s", r.baseURL)
	}
	desc := r.apiSpec.Info.Description
	if desc == "" {
		desc = r.apiSpec.Info.Title
	}
	return fmt.Sprintf("%s (v%s) - %d endpoints available", desc, r.apiSpec.Info.Version, len(r.apiSpec.Paths))
}
