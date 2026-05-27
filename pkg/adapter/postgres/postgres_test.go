package postgres

import (
	"context"
	"os"
	"testing"
)

func TestNew(t *testing.T) {
	a, err := New("postgresql://user:pass@localhost:5432/testdb?sslmode=disable")
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	if a == nil {
		t.Fatal("New() returned nil adapter")
	}
}

func TestPostgresAdapter_Name(t *testing.T) {
	a, _ := New("postgresql://user:pass@localhost:5432/testdb?sslmode=disable")
	if got := a.Name(); got != "PostgreSQL" {
		t.Errorf("Name() = %q, want %q", got, "PostgreSQL")
	}
}

func TestPostgresAdapter_Connect_InvalidDSN(t *testing.T) {
	a, _ := New("not-a-valid-dsn")
	err := a.Connect(context.Background())
	if err == nil {
		t.Error("Connect() with invalid DSN: expected error, got nil")
	}
}

func TestPostgresAdapter_Close_NilDB(t *testing.T) {
	// Close on a never-connected adapter must not panic and must return nil.
	a := &PostgresAdapter{dsn: "postgresql://x"}
	if err := a.Close(context.Background()); err != nil {
		t.Errorf("Close() on nil db returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ExecuteProcedure — validation guards (no DB required)
// ---------------------------------------------------------------------------

func TestExecuteProcedure_InvalidProcName(t *testing.T) {
	cases := []string{
		"",
		"1bad",
		"drop; table",
		"proc name",
		"proc--name",
		"../escape",
	}
	a := &PostgresAdapter{} // db is nil; validation fires before any DB call
	for _, name := range cases {
		_, err := a.ExecuteProcedure(context.Background(), name, nil)
		if err == nil {
			t.Errorf("ExecuteProcedure(%q): expected validation error, got nil", name)
		}
	}
}

func TestSafeProcName(t *testing.T) {
	valid := []string{"my_proc", "schema.my_proc", "_internal", "CamelCase", "proc123"}
	for _, name := range valid {
		if !safeProcName.MatchString(name) {
			t.Errorf("safeProcName: valid name %q rejected", name)
		}
	}
	invalid := []string{"", "1bad", "drop; table", "proc name", "proc--name", "../escape"}
	for _, name := range invalid {
		if safeProcName.MatchString(name) {
			t.Errorf("safeProcName: invalid name %q accepted", name)
		}
	}
}

func TestSafeParamName(t *testing.T) {
	valid := []string{"start_date", "end_date", "_private", "Param1", "x"}
	for _, name := range valid {
		if !safeParamName.MatchString(name) {
			t.Errorf("safeParamName: valid name %q rejected", name)
		}
	}
	invalid := []string{"bad param", "123start", "semi;colon", "quote'name", ""}
	for _, name := range invalid {
		if safeParamName.MatchString(name) {
			t.Errorf("safeParamName: invalid name %q accepted", name)
		}
	}
}

func TestExecuteProcedure_InvalidParamName(t *testing.T) {
	cases := []string{
		"bad param",
		"123start",
		"semi;colon",
		"quote'name",
	}
	a := &PostgresAdapter{}
	for _, paramName := range cases {
		_, err := a.ExecuteProcedure(context.Background(), "valid_proc", map[string]string{paramName: "v"})
		if err == nil {
			t.Errorf("ExecuteProcedure with param %q: expected validation error, got nil", paramName)
		}
	}
}


// ---------------------------------------------------------------------------
// Integration tests — skipped unless POSTGRES_TEST_DSN is set
// ---------------------------------------------------------------------------

func TestIntegration_ConnectAndQuery(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set")
	}

	a, err := New(dsn)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	ctx := context.Background()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect() failed: %v", err)
	}
	defer a.Close(ctx) //nolint:errcheck

	result, err := a.(*PostgresAdapter).ExecuteQuery(ctx, "SELECT 1 AS n")
	if err != nil {
		t.Fatalf("ExecuteQuery() failed: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(result.Rows))
	}
}

func TestIntegration_GetSchema(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set")
	}

	a, _ := New(dsn)
	ctx := context.Background()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect() failed: %v", err)
	}
	defer a.Close(ctx) //nolint:errcheck

	tables, err := a.(*PostgresAdapter).GetSchema(ctx)
	if err != nil {
		t.Fatalf("GetSchema() failed: %v", err)
	}
	// No assertion on count — schema may be empty in a fresh DB.
	// Just verify it doesn't error and returns a non-nil slice.
	if tables == nil {
		t.Error("GetSchema() returned nil")
	}
}

func TestIntegration_GetViews(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set")
	}

	a, _ := New(dsn)
	ctx := context.Background()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect() failed: %v", err)
	}
	defer a.Close(ctx) //nolint:errcheck

	views, err := a.(*PostgresAdapter).GetViews(ctx)
	if err != nil {
		t.Fatalf("GetViews() failed: %v", err)
	}
	if views == nil {
		t.Error("GetViews() returned nil")
	}
}

func TestIntegration_GetProcedures(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_TEST_DSN not set")
	}

	a, _ := New(dsn)
	ctx := context.Background()
	if err := a.Connect(ctx); err != nil {
		t.Fatalf("Connect() failed: %v", err)
	}
	defer a.Close(ctx) //nolint:errcheck

	procs, err := a.(*PostgresAdapter).GetProcedures(ctx)
	if err != nil {
		t.Fatalf("GetProcedures() failed: %v", err)
	}
	if procs == nil {
		t.Error("GetProcedures() returned nil")
	}
}
