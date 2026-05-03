package server

import (
	"context"
	"strings"
	"testing"

	"github.com/corebasehq/coremcp/pkg/adapter/dummy"
	"github.com/corebasehq/coremcp/pkg/core"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestHandleListTables(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	mcpSrv.AddSource("test_db", src, true)

	// Create request
	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name": "test_db",
	}

	ctx := context.Background()
	result, err := mcpSrv.handleListTables(ctx, request)

	if err != nil {
		t.Fatalf("handleListTables failed: %v", err)
	}

	if result == nil {
		t.Fatal("Result is nil")
	}

	// Check if result contains expected tables
	resultText := getResultText(result)
	if !strings.Contains(resultText, "users") || !strings.Contains(resultText, "orders") {
		t.Error("Result should contain 'users' and 'orders' tables")
	}
}

func TestHandleDescribeTable(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	mcpSrv.AddSource("test_db", src, true)

	// Create request
	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name": "test_db",
		"table_name":  "users",
	}

	ctx := context.Background()
	result, err := mcpSrv.handleDescribeTable(ctx, request)

	if err != nil {
		t.Fatalf("handleDescribeTable failed: %v", err)
	}

	if result == nil {
		t.Fatal("Result is nil")
	}

	resultText := getResultText(result)

	// Check for expected content
	expectedStrings := []string{
		"Table: users",
		"Primary Key",
		"Columns",
		"id",
		"username",
		"email",
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(resultText, expected) {
			t.Errorf("Result missing expected string: %q", expected)
		}
	}
}

func TestAddCustomTool(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	mcpSrv.AddSource("test_db", src, true)

	// Add a custom tool with a typed parameter
	err := mcpSrv.AddCustomTool(
		"get_user_orders",
		"Get orders for a specific user",
		"test_db",
		"SELECT * FROM orders WHERE user_id = {{user_id}}",
		[]ToolParam{{Name: "user_id", Type: "integer", Required: true}},
	)

	if err != nil {
		t.Fatalf("AddCustomTool failed: %v", err)
	}

	// Check if tool was registered
	if _, exists := mcpSrv.customTools["get_user_orders"]; !exists {
		t.Error("Custom tool was not registered")
	}

	// Verify tool config
	toolEntry := mcpSrv.customTools["get_user_orders"]
	if toolEntry.sourceName != "test_db" {
		t.Error("Tool source name mismatch")
	}
	if !strings.Contains(toolEntry.query, "user_id") {
		t.Error("Tool query should contain user_id parameter")
	}
}

func TestDescribeTableNotFound(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	mcpSrv.AddSource("test_db", src, true)

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name": "test_db",
		"table_name":  "nonexistent_table",
	}

	ctx := context.Background()
	result, err := mcpSrv.handleDescribeTable(ctx, request)

	if err != nil {
		t.Fatalf("handleDescribeTable failed: %v", err)
	}

	resultText := getResultText(result)
	if !strings.Contains(resultText, "not found") && !strings.Contains(resultText, "Table not found") {
		t.Error("Should return error for nonexistent table")
	}
}

// Helper function to extract text from CallToolResult
func getResultText(result *mcp.CallToolResult) string {
	if len(result.Content) == 0 {
		return ""
	}

	for _, content := range result.Content {
		if textContent, ok := content.(mcp.TextContent); ok {
			return textContent.Text
		}
	}

	return ""
}

// spySource wraps a real Source and records the last query sent to ExecuteQuery.
type spySource struct {
	core.Source
	lastQuery string
}

func (s *spySource) ExecuteQuery(ctx context.Context, query string, args ...any) (*core.QueryResult, error) {
	s.lastQuery = query
	return s.Source.ExecuteQuery(ctx, query, args...)
}

func TestCustomToolRouting(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	inner, _ := dummy.New("dummy://test")
	if err := inner.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	spy := &spySource{Source: inner}
	mcpSrv.AddSource("test_db", spy, true)

	// Register two tools that share a parameter name — routing must not confuse them.
	if err := mcpSrv.AddCustomTool("tool_a", "Tool A", "test_db",
		"SELECT * FROM orders WHERE user_id = {{id}}",
		[]ToolParam{{Name: "id", Type: "integer", Required: true}}); err != nil {
		t.Fatalf("AddCustomTool tool_a failed: %v", err)
	}
	if err := mcpSrv.AddCustomTool("tool_b", "Tool B", "test_db",
		"SELECT * FROM users WHERE id = {{id}}",
		[]ToolParam{{Name: "id", Type: "integer", Required: true}}); err != nil {
		t.Fatalf("AddCustomTool tool_b failed: %v", err)
	}

	ctx := context.Background()

	// Call tool_a and verify it runs the orders query.
	reqA := mcp.CallToolRequest{}
	reqA.Params.Arguments = map[string]interface{}{"id": "1"}
	resultA, err := mcpSrv.handleNamedCustomTool(ctx, reqA, "tool_a")
	if err != nil {
		t.Fatalf("handleNamedCustomTool tool_a error: %v", err)
	}
	if resultA == nil {
		t.Fatal("tool_a result is nil")
	}
	if !strings.Contains(spy.lastQuery, "orders") {
		t.Errorf("tool_a should query orders table, but executed: %s", spy.lastQuery)
	}

	// Call tool_b and verify it runs the users query.
	reqB := mcp.CallToolRequest{}
	reqB.Params.Arguments = map[string]interface{}{"id": "2"}
	resultB, err := mcpSrv.handleNamedCustomTool(ctx, reqB, "tool_b")
	if err != nil {
		t.Fatalf("handleNamedCustomTool tool_b error: %v", err)
	}
	if resultB == nil {
		t.Fatal("tool_b result is nil")
	}
	if !strings.Contains(spy.lastQuery, "users") {
		t.Errorf("tool_b should query users table, but executed: %s", spy.lastQuery)
	}
}

func TestCustomToolSQLInjectionBlocked(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	mcpSrv.AddSource("test_db", src, true)

	// Integer-typed parameter — only bare integers are accepted.
	if err := mcpSrv.AddCustomTool("get_user", "Get user", "test_db",
		"SELECT * FROM users WHERE id = {{id}}",
		[]ToolParam{{Name: "id", Type: "integer", Required: true}}); err != nil {
		t.Fatalf("AddCustomTool failed: %v", err)
	}

	intInjections := []string{
		"1 OR 1=1", // logic-injection without quotes/semicolons — previously bypassed denylist
		"1; DROP TABLE users",
		"1' OR '1'='1",
		"1 -- comment",
		"1 /* block */",
		"(SELECT 1)",
		"1 UNION SELECT 1",
	}

	ctx := context.Background()
	for _, payload := range intInjections {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]interface{}{"id": payload}
		result, err := mcpSrv.handleNamedCustomTool(ctx, req, "get_user")
		if err != nil {
			t.Fatalf("unexpected Go error for payload %q: %v", payload, err)
		}
		text := getResultText(result)
		if !strings.Contains(text, "Invalid parameter") {
			t.Errorf("integer injection payload %q was not blocked, got: %s", payload, text)
		}
	}

	// String-typed parameter — single quotes must be escaped, not stripped.
	if err := mcpSrv.AddCustomTool("get_order", "Get order by status", "test_db",
		"SELECT * FROM orders WHERE status = '{{status}}'",
		[]ToolParam{{Name: "status", Type: "string", Required: true}}); err != nil {
		t.Fatalf("AddCustomTool get_order failed: %v", err)
	}

	// A value with a quote must be coerced (doubled), not rejected.
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"status": "O'Brien"}
	result, err := mcpSrv.handleNamedCustomTool(ctx, req, "get_order")
	if err != nil {
		t.Fatalf("unexpected Go error for quoted string: %v", err)
	}
	// Should succeed (dummy source returns rows) or at least not return "Invalid parameter".
	text := getResultText(result)
	if strings.Contains(text, "Invalid parameter") {
		t.Errorf("valid string with quote was unexpectedly rejected: %s", text)
	}
}

func TestHandleListViews(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	mcpSrv.AddSource("test_db", src, true)

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name": "test_db",
	}

	ctx := context.Background()
	result, err := mcpSrv.handleListViews(ctx, request)
	if err != nil {
		t.Fatalf("handleListViews failed: %v", err)
	}
	if result == nil {
		t.Fatal("Result is nil")
	}

	resultText := getResultText(result)
	expectedStrings := []string{
		"Views in test_db",
		"vw_customer_orders",
		"user_id",
		"order_count",
	}
	for _, s := range expectedStrings {
		if !strings.Contains(resultText, s) {
			t.Errorf("List views result missing expected string: %q", s)
		}
	}
}

func TestHandleListViewsSourceNotFound(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name": "nonexistent",
	}

	result, err := mcpSrv.handleListViews(context.Background(), request)
	if err != nil {
		t.Fatalf("handleListViews returned unexpected error: %v", err)
	}

	resultText := getResultText(result)
	if !strings.Contains(resultText, "Source not found") {
		t.Errorf("Expected 'Source not found' in result, got: %s", resultText)
	}
}

func TestHandleListProcedures(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	mcpSrv.AddSource("test_db", src, true)

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name": "test_db",
	}

	ctx := context.Background()
	result, err := mcpSrv.handleListProcedures(ctx, request)
	if err != nil {
		t.Fatalf("handleListProcedures failed: %v", err)
	}
	if result == nil {
		t.Fatal("Result is nil")
	}

	resultText := getResultText(result)
	expectedStrings := []string{
		"Stored Procedures in test_db",
		"sp_GetUserOrders",
		"sp_GetDailySummary",
		"@UserID",
		"@StartDate",
	}
	for _, s := range expectedStrings {
		if !strings.Contains(resultText, s) {
			t.Errorf("List procedures result missing expected string: %q", s)
		}
	}
}

func TestHandleExecuteProcedure(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	// readOnly=false to allow procedure execution
	mcpSrv.AddSource("test_db", src, false)

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name":    "test_db",
		"procedure_name": "sp_GetUserOrders",
		"params":         `{"UserID":"42","StatusFilter":"active"}`,
	}

	ctx := context.Background()
	result, err := mcpSrv.handleExecuteProcedure(ctx, request)
	if err != nil {
		t.Fatalf("handleExecuteProcedure failed: %v", err)
	}
	if result == nil {
		t.Fatal("Result is nil")
	}

	resultText := getResultText(result)
	if !strings.Contains(resultText, "sp_GetUserOrders") {
		t.Errorf("Execute procedure result missing procedure name, got: %s", resultText)
	}
}

func TestHandleExecuteProcedureReadOnly(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	// readOnly=true — execution must be blocked
	mcpSrv.AddSource("test_db", src, true)

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name":    "test_db",
		"procedure_name": "sp_GetUserOrders",
	}

	result, err := mcpSrv.handleExecuteProcedure(context.Background(), request)
	if err != nil {
		t.Fatalf("handleExecuteProcedure returned unexpected Go error: %v", err)
	}

	resultText := getResultText(result)
	if !strings.Contains(resultText, "read-only") {
		t.Errorf("Expected read-only error, got: %s", resultText)
	}
}

func TestHandleExecuteProcedureInvalidJSON(t *testing.T) {
	mcpSrv := NewMCPServer("test-server", "1.0.0")

	src, _ := dummy.New("dummy://test")
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect source: %v", err)
	}
	mcpSrv.AddSource("test_db", src, false)

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"source_name":    "test_db",
		"procedure_name": "sp_GetUserOrders",
		"params":         `{invalid json`,
	}

	result, err := mcpSrv.handleExecuteProcedure(context.Background(), request)
	if err != nil {
		t.Fatalf("handleExecuteProcedure returned unexpected Go error: %v", err)
	}

	resultText := getResultText(result)
	if !strings.Contains(resultText, "Invalid params JSON") {
		t.Errorf("Expected JSON parse error, got: %s", resultText)
	}
}
