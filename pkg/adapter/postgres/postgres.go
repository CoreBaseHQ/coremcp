// Package postgres provides a PostgreSQL database adapter for CoreMCP.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/corebasehq/coremcp/pkg/core"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresAdapter implements the core.Source interface for PostgreSQL.
type PostgresAdapter struct {
	dsn string
	db  *sql.DB
}

// New creates a new PostgreSQL adapter.
// DSN format: postgresql://user:password@host:port/dbname?sslmode=disable
func New(dsn string) (core.Source, error) {
	return &PostgresAdapter{dsn: dsn}, nil
}

func (p *PostgresAdapter) Name() string {
	return "PostgreSQL"
}

func (p *PostgresAdapter) Connect(ctx context.Context) error {
	db, err := sql.Open("pgx", p.dsn)
	if err != nil {
		return err
	}
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("PostgreSQL ping error: %w", err)
	}
	p.db = db
	return nil
}

func (p *PostgresAdapter) Close(_ context.Context) error {
	if p.db != nil {
		return p.db.Close()
	}
	return nil
}

// GetSchema retrieves all user tables with columns, PKs, FKs, and column comments.
// Column comments are sourced from pg_description (COMMENT ON COLUMN).
func (p *PostgresAdapter) GetSchema(ctx context.Context) ([]core.TableSchema, error) {
	tableRows, err := p.db.QueryContext(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		  AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`)
	if err != nil {
		return nil, err
	}
	defer tableRows.Close() //nolint:errcheck

	tableMap := make(map[string]*core.TableSchema)
	var tableNames []string

	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			return nil, err
		}
		tableMap[name] = &core.TableSchema{
			Name:        name,
			Columns:     []core.ColumnInfo{},
			ForeignKeys: []core.ForeignKey{},
			PrimaryKeys: []string{},
		}
		tableNames = append(tableNames, name)
	}
	if err := tableRows.Err(); err != nil {
		return nil, err
	}

	colRows, err := p.db.QueryContext(ctx, `
		SELECT
			c.table_name,
			c.column_name,
			c.data_type,
			c.is_nullable,
			COALESCE(d.description, '') AS description
		FROM information_schema.columns c
		LEFT JOIN pg_class cl ON cl.relname = c.table_name
		LEFT JOIN pg_namespace n ON n.oid = cl.relnamespace AND n.nspname = c.table_schema
		LEFT JOIN pg_attribute a ON a.attrelid = cl.oid AND a.attname = c.column_name
		LEFT JOIN pg_description d ON d.objoid = cl.oid AND d.objsubid = a.attnum
		WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY c.table_name, c.ordinal_position
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to load column metadata: %w", err)
	}
	defer colRows.Close() //nolint:errcheck

	for colRows.Next() {
		var tableName string
		var col core.ColumnInfo
		var isNullable, description string
		if err := colRows.Scan(&tableName, &col.Name, &col.DataType, &isNullable, &description); err != nil {
			return nil, err
		}
		if t, ok := tableMap[tableName]; ok {
			col.IsNullable = isNullable == "YES"
			col.Description = description
			t.Columns = append(t.Columns, col)
		}
	}
	if err := colRows.Err(); err != nil {
		return nil, err
	}

	pkRows, err := p.db.QueryContext(ctx, `
		SELECT kcu.table_name, kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON tc.constraint_name = kcu.constraint_name
			AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND tc.table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY kcu.table_name, kcu.ordinal_position
	`)
	if err == nil {
		defer pkRows.Close() //nolint:errcheck
		for pkRows.Next() {
			var tableName, colName string
			if err := pkRows.Scan(&tableName, &colName); err == nil {
				if t, ok := tableMap[tableName]; ok {
					t.PrimaryKeys = append(t.PrimaryKeys, colName)
				}
			}
		}
		_ = pkRows.Err()
	}

	fkRows, err := p.db.QueryContext(ctx, `
		SELECT
			kcu.table_name,
			tc.constraint_name,
			kcu.column_name,
			ccu.table_name  AS referenced_table,
			ccu.column_name AS referenced_column
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
			ON tc.constraint_name = kcu.constraint_name
			AND tc.table_schema = kcu.table_schema
		JOIN information_schema.referential_constraints rc
			ON tc.constraint_name = rc.constraint_name
			AND tc.table_schema = rc.constraint_schema
		JOIN information_schema.constraint_column_usage ccu
			ON rc.unique_constraint_name = ccu.constraint_name
			AND rc.unique_constraint_schema = ccu.constraint_schema
		WHERE tc.constraint_type = 'FOREIGN KEY'
		  AND tc.table_schema NOT IN ('pg_catalog', 'information_schema')
	`)
	if err == nil {
		defer fkRows.Close() //nolint:errcheck
		for fkRows.Next() {
			var tableName string
			var fk core.ForeignKey
			if err := fkRows.Scan(&tableName, &fk.ConstraintName, &fk.ColumnName, &fk.ReferencedTable, &fk.ReferencedColumn); err == nil {
				if t, ok := tableMap[tableName]; ok {
					t.ForeignKeys = append(t.ForeignKeys, fk)
				}
			}
		}
		_ = fkRows.Err()
	}

	tables := make([]core.TableSchema, 0, len(tableNames))
	for _, name := range tableNames {
		if t, ok := tableMap[name]; ok {
			tables = append(tables, *t)
		}
	}
	return tables, nil
}

// GetViews retrieves all user-defined views with their column definitions.
func (p *PostgresAdapter) GetViews(ctx context.Context) ([]core.ViewSchema, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT table_schema, table_name
		FROM information_schema.views
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY table_schema, table_name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	viewMap := make(map[string]*core.ViewSchema)
	var viewNames []string

	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, err
		}
		key := schema + "." + name
		viewMap[key] = &core.ViewSchema{Name: key, Columns: []core.ColumnInfo{}}
		viewNames = append(viewNames, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(viewNames) == 0 {
		return []core.ViewSchema{}, nil
	}

	colRows, err := p.db.QueryContext(ctx, `
		SELECT c.table_schema, c.table_name, c.column_name, c.data_type, c.is_nullable
		FROM information_schema.columns c
		INNER JOIN information_schema.views v
			ON c.table_name = v.table_name AND c.table_schema = v.table_schema
		WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY c.table_schema, c.table_name, c.ordinal_position
	`)
	if err != nil {
		return nil, err
	}
	defer colRows.Close() //nolint:errcheck

	for colRows.Next() {
		var schema, tableName string
		var col core.ColumnInfo
		var nullable string
		if err := colRows.Scan(&schema, &tableName, &col.Name, &col.DataType, &nullable); err != nil {
			return nil, err
		}
		key := schema + "." + tableName
		if v, ok := viewMap[key]; ok {
			col.IsNullable = nullable == "YES"
			v.Columns = append(v.Columns, col)
		}
	}
	if err := colRows.Err(); err != nil {
		return nil, err
	}

	views := make([]core.ViewSchema, 0, len(viewNames))
	for _, key := range viewNames {
		if v, ok := viewMap[key]; ok {
			views = append(views, *v)
		}
	}
	return views, nil
}

// GetProcedures retrieves stored procedures and functions from the database.
// Both FUNCTION and PROCEDURE routine types are returned (PG 11+ has native PROCEDUREs).
func (p *PostgresAdapter) GetProcedures(ctx context.Context) ([]core.StoredProcedure, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT specific_name, routine_name, routine_type
		FROM information_schema.routines
		WHERE routine_schema NOT IN ('pg_catalog', 'information_schema')
		  AND routine_type IN ('FUNCTION', 'PROCEDURE')
		ORDER BY routine_name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	// specificMap is keyed by specific_name (unique per overload) for parameter join.
	// procMap is keyed by routine_name for de-duplication of overloaded functions.
	specificMap := make(map[string]*core.StoredProcedure)
	procMap := make(map[string]*core.StoredProcedure)
	var procNames []string

	for rows.Next() {
		var specificName, routineName, routineType string
		if err := rows.Scan(&specificName, &routineName, &routineType); err != nil {
			return nil, err
		}
		proc, exists := procMap[routineName]
		if !exists {
			proc = &core.StoredProcedure{
				Name:        routineName,
				Description: routineType,
				Parameters:  []core.ProcParameter{},
			}
			procMap[routineName] = proc
			procNames = append(procNames, routineName)
		}
		specificMap[specificName] = proc
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(procNames) == 0 {
		return []core.StoredProcedure{}, nil
	}

	paramRows, err := p.db.QueryContext(ctx, `
		SELECT specific_name, parameter_name, data_type, parameter_mode
		FROM information_schema.parameters
		WHERE specific_schema NOT IN ('pg_catalog', 'information_schema')
		  AND parameter_name IS NOT NULL
		  AND parameter_name != ''
		ORDER BY specific_name, ordinal_position
	`)
	if err != nil {
		return nil, err
	}
	defer paramRows.Close() //nolint:errcheck

	for paramRows.Next() {
		var specificName, paramName, dataType, mode string
		if err := paramRows.Scan(&specificName, &paramName, &dataType, &mode); err != nil {
			return nil, err
		}
		if proc, ok := specificMap[specificName]; ok {
			proc.Parameters = append(proc.Parameters, core.ProcParameter{
				Name:     paramName,
				DataType: dataType,
				Mode:     mode,
			})
		}
	}
	if err := paramRows.Err(); err != nil {
		return nil, err
	}

	procs := make([]core.StoredProcedure, 0, len(procNames))
	for _, name := range procNames {
		if proc, ok := procMap[name]; ok {
			procs = append(procs, *proc)
		}
	}
	return procs, nil
}

var safeProcName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]*$`)
var safeParamName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ExecuteQuery runs a SQL query against the PostgreSQL database.
// PostgreSQL natively supports LIMIT N so no rewrite is needed.
func (p *PostgresAdapter) ExecuteQuery(ctx context.Context, query string, args ...any) (*core.QueryResult, error) {
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return parseRows(rows)
}

// ExecuteProcedure calls a stored procedure or function by name with named parameters.
// Parameters are passed as positional args ($1, $2, ...) sorted alphabetically by name.
// Attempts CALL first (PG 11+ PROCEDURE), falls back to SELECT * FROM for functions.
func (p *PostgresAdapter) ExecuteProcedure(ctx context.Context, name string, params map[string]string) (*core.QueryResult, error) {
	if !safeProcName.MatchString(name) {
		return nil, fmt.Errorf("invalid procedure name %q: only letters, digits, underscores and dots are allowed", name)
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		if !safeParamName.MatchString(k) {
			return nil, fmt.Errorf("invalid parameter name %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make([]any, len(keys))
	parts := make([]string, len(keys))
	for i, k := range keys {
		args[i] = params[k]
		parts[i] = fmt.Sprintf("%s => $%d", k, i+1)
	}

	argList := strings.Join(parts, ", ")

	// Try CALL first (PostgreSQL 11+ native PROCEDURE).
	// name and argList are built exclusively from regex-validated identifiers + $N placeholders;
	// actual values are bound via args — no injection possible. //nolint:gosec
	callSQL := fmt.Sprintf("CALL %s(%s)", name, argList) //nolint:gosec
	rows, err := p.db.QueryContext(ctx, callSQL, args...)
	if err == nil {
		defer rows.Close() //nolint:errcheck
		return parseRows(rows)
	}
	callErr := err

	// Fallback: treat as a FUNCTION and call via SELECT.
	selectSQL := fmt.Sprintf("SELECT * FROM %s(%s)", name, argList) //nolint:gosec
	funcRows, funcErr := p.db.QueryContext(ctx, selectSQL, args...)
	if funcErr != nil {
		return nil, fmt.Errorf("procedure execution failed: %w", callErr)
	}
	defer funcRows.Close() //nolint:errcheck
	return parseRows(funcRows)
}

func parseRows(rows *sql.Rows) (*core.QueryResult, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	count := len(columns)
	values := make([]any, count)
	valuePtrs := make([]any, count)
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	var finalRows []map[string]any
	for rows.Next() {
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}
		rowMap := make(map[string]any, count)
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			rowMap[col] = val
		}
		finalRows = append(finalRows, rowMap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &core.QueryResult{Columns: columns, Rows: finalRows}, nil
}
