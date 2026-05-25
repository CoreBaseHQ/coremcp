// Package mssql provides a Microsoft SQL Server database adapter.
package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/corebasehq/coremcp/pkg/core"
	"github.com/corebasehq/coremcp/pkg/turkish"
	_ "github.com/microsoft/go-mssqldb"
)

// MSSQLAdapter implements the core.Source interface for Microsoft SQL Server.
type MSSQLAdapter struct {
	dsn              string
	db               *sql.DB
	serverMajor      int  // Detected SQL Server major version: 8=2000, 9=2005, 10=2008, 11=2012, 15=2019, etc.
	noLock           bool // When true, queries run under READ UNCOMMITTED isolation (equivalent to WITH (NOLOCK))
	normalizeTurkish bool // When true, Turkish chars in SQL literals are normalized to ASCII uppercase
}

// New creates a new MSSQL adapter with the given DSN.
// DSN format: sqlserver://username:password@host:port?database=dbname
//
// When noLock is true, all SELECT queries are executed under READ UNCOMMITTED
// transaction isolation level, which is equivalent to applying WITH (NOLOCK)
// to every table in the query. This prevents read locks and improves throughput
// on busy OLTP databases at the cost of potentially reading uncommitted data.
//
// When normalizeTurkish is true, the adapter activates the Turkish normalization
// middleware:
//   - Outgoing: Turkish characters inside SQL string literals are converted to
//     their ASCII uppercase equivalents before the query is sent to the database.
//     This enables AI-generated queries to match data in legacy Turkish ERP systems
//     that store text as uppercase ASCII (e.g. "HUSEYIN" instead of "Hüseyin").
//   - Incoming: Common Windows-1254 → Windows-1252 mojibake patterns in result
//     strings are corrected automatically.
func New(dsn string, noLock bool, normalizeTurkish bool) (core.Source, error) {
	return &MSSQLAdapter{dsn: dsn, noLock: noLock, normalizeTurkish: normalizeTurkish}, nil
}

func (m *MSSQLAdapter) Name() string {
	if m.serverMajor > 0 {
		return fmt.Sprintf("MSSQL (%s)", m.serverVersionLabel())
	}
	return "MSSQL"
}

// serverVersionLabel returns a human-readable SQL Server release name.
func (m *MSSQLAdapter) serverVersionLabel() string {
	switch m.serverMajor {
	case 8:
		return "SQL Server 2000"
	case 9:
		return "SQL Server 2005"
	case 10:
		return "SQL Server 2008/2008 R2"
	case 11:
		return "SQL Server 2012"
	case 12:
		return "SQL Server 2014"
	case 13:
		return "SQL Server 2016"
	case 14:
		return "SQL Server 2017"
	case 15:
		return "SQL Server 2019"
	case 16:
		return "SQL Server 2022"
	default:
		return fmt.Sprintf("unknown v%d.x", m.serverMajor)
	}
}

// isPreSQL2005 returns true for SQL Server 2000 (major version ≤ 8).
// SQL Server 2000 lacks sys.* catalog views (sys.tables, sys.foreign_keys,
// sys.extended_properties) and requires legacy sysobjects/syscolumns queries.
func (m *MSSQLAdapter) isPreSQL2005() bool {
	return m.serverMajor > 0 && m.serverMajor < 9
}

// isPreSQL2012 returns true for SQL Server versions before 2012 (major < 11).
// These versions do not support the OFFSET … FETCH NEXT pagination syntax;
// row-limiting must be done with SELECT TOP N instead.
func (m *MSSQLAdapter) isPreSQL2012() bool {
	return m.serverMajor > 0 && m.serverMajor < 11
}

func (m *MSSQLAdapter) Connect(ctx context.Context) error {
	// DSN format: sqlserver://username:password@host:port?database=dbname
	db, err := sql.Open("sqlserver", m.dsn)
	if err != nil {
		return err
	}

	// Test the connection
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("MSSQL ping error: %w", err)
	}

	m.db = db

	// Detect the server version (e.g. "8.00.2039" for SQL Server 2000,
	// "10.50.6000.34" for SQL Server 2008 R2, "15.0.2000.5" for SQL Server 2019).
	// The major version number drives version-specific query branches.
	var productVersion string
	if err := m.db.QueryRowContext(ctx,
		"SELECT CAST(SERVERPROPERTY('ProductVersion') AS NVARCHAR(128))").Scan(&productVersion); err == nil {
		if parts := strings.SplitN(productVersion, ".", 2); len(parts) > 0 {
			if major, convErr := strconv.Atoi(parts[0]); convErr == nil {
				m.serverMajor = major
			}
		}
	}
	// Default to a modern version when detection fails so queries use current syntax.
	if m.serverMajor == 0 {
		m.serverMajor = 15 // SQL Server 2019
	}

	return nil
}

// Close closes the underlying *sql.DB. The ctx parameter is accepted to satisfy
// the core.Source interface but is not honoured: database/sql's Close is a
// bounded, in-process operation and does not accept a context.
func (m *MSSQLAdapter) Close(_ context.Context) error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}

func (m *MSSQLAdapter) GetSchema(ctx context.Context) ([]core.TableSchema, error) {
	var query string
	if m.isPreSQL2005() {
		// SQL Server 2000: sys.tables does not exist.
		// sysobjects with xtype = 'U' enumerates all user-defined tables.
		query = `
			SELECT name
			FROM sysobjects
			WHERE xtype = 'U'
			ORDER BY name
		`
	} else {
		query = `
			SELECT TABLE_NAME
			FROM INFORMATION_SCHEMA.TABLES
			WHERE TABLE_TYPE = 'BASE TABLE'
			ORDER BY TABLE_NAME
		`
	}

	rows, err := m.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tableMap := make(map[string]*core.TableSchema)
	var tableNames []string

	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			return nil, err
		}
		tableMap[tableName] = &core.TableSchema{
			Name:        tableName,
			Columns:     []core.ColumnInfo{},
			ForeignKeys: []core.ForeignKey{},
			PrimaryKeys: []string{},
		}
		tableNames = append(tableNames, tableName)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Get columns with descriptions.
	// We run one generic query for all tables.
	var columnQuery string
	if m.isPreSQL2005() {
		columnQuery = `
			SELECT
				TABLE_NAME,
				COLUMN_NAME,
				DATA_TYPE,
				IS_NULLABLE,
				CAST('' AS NVARCHAR(4000)) AS DESCRIPTION
			FROM INFORMATION_SCHEMA.COLUMNS
			ORDER BY TABLE_NAME, ORDINAL_POSITION
		`
	} else {
		columnQuery = `
			SELECT
				c.TABLE_NAME,
				c.COLUMN_NAME,
				c.DATA_TYPE,
				c.IS_NULLABLE,
				ISNULL(CAST(ep.value AS NVARCHAR(4000)), '') as DESCRIPTION
			FROM INFORMATION_SCHEMA.COLUMNS c
			LEFT JOIN sys.extended_properties ep
				ON ep.major_id = OBJECT_ID(c.TABLE_SCHEMA + '.' + c.TABLE_NAME)
				AND ep.minor_id = c.ORDINAL_POSITION
				AND ep.name = 'MS_Description'
			ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION
		`
	}

	colRows, err := m.db.QueryContext(ctx, columnQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to load column metadata: %w", err)
	}
	defer colRows.Close()
	for colRows.Next() {
		var tableName string
		var col core.ColumnInfo
		var isNullable string
		var description sql.NullString

		if err := colRows.Scan(&tableName, &col.Name, &col.DataType, &isNullable, &description); err != nil {
			return nil, err
		}

		if t, ok := tableMap[tableName]; ok {
			col.IsNullable = (isNullable == "YES")
			if description.Valid {
				col.Description = description.String
			}
			t.Columns = append(t.Columns, col)
		}
	}
	if err := colRows.Err(); err != nil {
		return nil, err
	}

	// Get primary keys for all tables
	pkQuery := `
		SELECT TABLE_NAME, COLUMN_NAME
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		WHERE OBJECTPROPERTY(OBJECT_ID(CONSTRAINT_SCHEMA + '.' + CONSTRAINT_NAME), 'IsPrimaryKey') = 1
		ORDER BY TABLE_NAME, ORDINAL_POSITION
	`
	pkRows, err := m.db.QueryContext(ctx, pkQuery)
	if err == nil {
		defer pkRows.Close()
		for pkRows.Next() {
			var tableName, pkCol string
			if err := pkRows.Scan(&tableName, &pkCol); err == nil {
				if t, ok := tableMap[tableName]; ok {
					t.PrimaryKeys = append(t.PrimaryKeys, pkCol)
				}
			}
		}
		_ = pkRows.Err()
	}

	// Get foreign keys for all tables
	var fkQuery string
	if m.isPreSQL2005() {
		fkQuery = `
			SELECT
				OBJECT_NAME(fk.fkeyid)            AS TABLE_NAME,
				OBJECT_NAME(fk.constid)           AS FK_NAME,
				COL_NAME(fk.fkeyid, fk.fkey)      AS COLUMN_NAME,
				OBJECT_NAME(fk.rkeyid)            AS REFERENCED_TABLE,
				COL_NAME(fk.rkeyid, fk.rkey)      AS REFERENCED_COLUMN
			FROM sysforeignkeys fk
		`
	} else {
		fkQuery = `
			SELECT
				OBJECT_NAME(fk.parent_object_id)   AS TABLE_NAME,
				fk.name                            AS FK_NAME,
				c1.name                            AS COLUMN_NAME,
				OBJECT_NAME(fk.referenced_object_id) AS REFERENCED_TABLE,
				c2.name                            AS REFERENCED_COLUMN
			FROM sys.foreign_keys fk
			INNER JOIN sys.foreign_key_columns fkc ON fk.object_id = fkc.constraint_object_id
			INNER JOIN sys.columns c1 ON fkc.parent_object_id = c1.object_id AND fkc.parent_column_id = c1.column_id
			INNER JOIN sys.columns c2 ON fkc.referenced_object_id = c2.object_id AND fkc.referenced_column_id = c2.column_id
		`
	}

	fkRows, err := m.db.QueryContext(ctx, fkQuery)
	if err == nil {
		defer fkRows.Close()
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

	var tables []core.TableSchema
	for _, name := range tableNames {
		if t, ok := tableMap[name]; ok {
			tables = append(tables, *t)
		}
	}

	return tables, nil
}

func (m *MSSQLAdapter) ExecuteQuery(ctx context.Context, query string, args ...any) (*core.QueryResult, error) {
	// Middleware 1: Turkish character normalization (outgoing)
	// Converts Turkish chars in SQL string literals to ASCII uppercase so that
	// queries generated by an AI assistant can match data stored as uppercase
	// ASCII in legacy Turkish ERP databases (Turkish_CI_AS collation).
	if m.normalizeTurkish {
		query = turkish.NormalizeSQLLiterals(query)
	}

	// Middleware 2: Version-aware query adaptation.
	// Rewrites MySQL-style LIMIT N → T-SQL TOP N so the security.QueryModifier
	// output is valid on all SQL Server versions.
	// On pre-2012 servers it also rewrites OFFSET…FETCH NEXT to TOP N.
	query = m.adaptQueryForVersion(query)

	// Middleware 3: NOLOCK — use READ UNCOMMITTED isolation level.
	// This is equivalent to adding WITH (NOLOCK) to every table reference
	// and prevents SELECT queries from acquiring shared locks.
	if m.noLock {
		return m.executeQueryReadUncommitted(ctx, query, args...)
	}

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return m.parseRows(rows)
}

// ---------------------------------------------------------------------------
// Query adaptation helpers
// ---------------------------------------------------------------------------

// hasTopRe detects an existing SELECT TOP N clause to avoid double-injection.
var hasTopRe = regexp.MustCompile(`(?i)\bTOP\s*[\(\d]`)

// limitClauseRe matches a trailing MySQL-style LIMIT N (the security.QueryModifier
// uses MySQL dialect via sqlparser and may append this to any SELECT).
var limitClauseRe = regexp.MustCompile(`(?i)^([\s\S]*?)\s+LIMIT\s+(\d+)\s*$`)

// selectKeywordRe matches the SELECT keyword so TOP N can be injected after it.
var selectKeywordRe = regexp.MustCompile(`(?i)^(\s*SELECT\s+)`)

// offsetFetchRe matches the SQL Server 2012+ OFFSET…FETCH NEXT pagination clause.
var offsetFetchRe = regexp.MustCompile(`(?i)\s+OFFSET\s+\d+\s+ROWS?\s+FETCH\s+NEXT\s+(\d+)\s+ROWS?\s+ONLY\s*$`)

// adaptQueryForVersion rewrites query syntax for the detected SQL Server version.
//
// Transformation 1 (all versions): MySQL-style "LIMIT N" → "SELECT TOP N …"
// The security.QueryModifier uses sqlparser (MySQL dialect) and may append
// "LIMIT N" to SELECT statements. T-SQL requires "SELECT TOP N" instead.
//
// Transformation 2 (pre-2012 only): "OFFSET x ROWS FETCH NEXT N ROWS ONLY" → "SELECT TOP N …"
// SQL Server 2012+ supports OFFSET/FETCH for pagination. Older servers need
// ROW_NUMBER() or TOP. When the AI (or a query rewriter) emits OFFSET/FETCH
// against a pre-2012 server, this strips the clause and replaces it with TOP N
// (the offset is lost, but for row-limiting purposes this is acceptable).
func (m *MSSQLAdapter) adaptQueryForVersion(query string) string {
	// Transformation 1: LIMIT N → SELECT TOP N
	query = convertLimitToTop(query)

	// Transformation 2: OFFSET…FETCH NEXT N → SELECT TOP N (pre-2012 only)
	if m.isPreSQL2012() {
		if mm := offsetFetchRe.FindStringSubmatch(query); mm != nil {
			without := strings.TrimSpace(offsetFetchRe.ReplaceAllString(query, ""))
			// Reuse convertLimitToTop by appending a synthetic LIMIT clause.
			query = convertLimitToTop(without + " LIMIT " + mm[1])
		}
	}

	return query
}

// convertLimitToTop rewrites a trailing MySQL-style "LIMIT N" to T-SQL "SELECT TOP N".
// Returns the query unchanged when it already has a TOP clause, has no LIMIT clause,
// or is not a SELECT statement (INSERT/UPDATE/DELETE are left as-is).
func convertLimitToTop(query string) string {
	if hasTopRe.MatchString(query) {
		// already has TOP — strip any spurious LIMIT appended by QueryModifier
		if m := limitClauseRe.FindStringSubmatch(query); m != nil {
			return strings.TrimSpace(m[1])
		}
		return query
	}
	m := limitClauseRe.FindStringSubmatch(query)
	if m == nil {
		return query // no LIMIT clause
	}
	withoutLimit := strings.TrimSpace(m[1])
	// Only rewrite SELECT statements; non-SELECT DML is left unchanged.
	if !selectKeywordRe.MatchString(withoutLimit) {
		return query
	}
	n := m[2]
	return selectKeywordRe.ReplaceAllString(withoutLimit, "${1}TOP "+n+" ")
}

// safeProcName validates that a stored procedure name contains only safe characters
// and prevents SQL injection through procedure names.
var safeProcName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_#@.]*$`)

// GetViews retrieves all views from the database along with their column information.
func (m *MSSQLAdapter) GetViews(ctx context.Context) ([]core.ViewSchema, error) {
	viewQuery := `
		SELECT TABLE_SCHEMA, TABLE_NAME
		FROM INFORMATION_SCHEMA.VIEWS
		ORDER BY TABLE_SCHEMA, TABLE_NAME
	`
	rows, err := m.db.QueryContext(ctx, viewQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	viewMap := make(map[string]*core.ViewSchema)
	var viewNames []string
	for rows.Next() {
		var schemaName string
		var name string
		if err := rows.Scan(&schemaName, &name); err != nil {
			return nil, err
		}
		key := schemaName + "." + name
		viewMap[key] = &core.ViewSchema{Name: key, Columns: []core.ColumnInfo{}}
		viewNames = append(viewNames, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(viewNames) == 0 {
		return []core.ViewSchema{}, nil
	}

	colQuery := `
		SELECT c.TABLE_SCHEMA, c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.IS_NULLABLE
		FROM INFORMATION_SCHEMA.COLUMNS c
		INNER JOIN INFORMATION_SCHEMA.VIEWS v
			ON c.TABLE_NAME = v.TABLE_NAME
			AND c.TABLE_SCHEMA = v.TABLE_SCHEMA
		ORDER BY c.TABLE_SCHEMA, c.TABLE_NAME, c.ORDINAL_POSITION
	`
	colRows, err := m.db.QueryContext(ctx, colQuery)
	if err != nil {
		return nil, err
	}
	defer colRows.Close()

	for colRows.Next() {
		var schemaName string
		var tableName string
		var col core.ColumnInfo
		var nullable string
		if err := colRows.Scan(&schemaName, &tableName, &col.Name, &col.DataType, &nullable); err != nil {
			return nil, err
		}
		key := schemaName + "." + tableName
		if v, ok := viewMap[key]; ok {
			col.IsNullable = (nullable == "YES")
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

// GetProcedures retrieves all stored procedures and their parameters from the database.
func (m *MSSQLAdapter) GetProcedures(ctx context.Context) ([]core.StoredProcedure, error) {
	spQuery := `
		SELECT ROUTINE_NAME
		FROM INFORMATION_SCHEMA.ROUTINES
		WHERE ROUTINE_TYPE = 'PROCEDURE'
		ORDER BY ROUTINE_NAME
	`
	rows, err := m.db.QueryContext(ctx, spQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	procMap := make(map[string]*core.StoredProcedure)
	var spNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		procMap[name] = &core.StoredProcedure{Name: name, Parameters: []core.ProcParameter{}}
		spNames = append(spNames, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(spNames) == 0 {
		return []core.StoredProcedure{}, nil
	}

	paramQuery := `
		SELECT
			SPECIFIC_NAME,
			PARAMETER_NAME,
			DATA_TYPE,
			PARAMETER_MODE
		FROM INFORMATION_SCHEMA.PARAMETERS
		WHERE PARAMETER_NAME <> ''
		ORDER BY SPECIFIC_NAME, ORDINAL_POSITION
	`
	paramRows, err := m.db.QueryContext(ctx, paramQuery)
	if err != nil {
		return nil, err
	}
	defer paramRows.Close()

	for paramRows.Next() {
		var procName string
		var p core.ProcParameter
		var paramName sql.NullString
		if err := paramRows.Scan(&procName, &paramName, &p.DataType, &p.Mode); err != nil {
			return nil, err
		}
		if !paramName.Valid || paramName.String == "" {
			continue
		}
		p.Name = paramName.String
		if proc, ok := procMap[procName]; ok {
			proc.Parameters = append(proc.Parameters, p)
		}
	}
	if err := paramRows.Err(); err != nil {
		return nil, err
	}

	procs := make([]core.StoredProcedure, 0, len(spNames))
	for _, name := range spNames {
		if proc, ok := procMap[name]; ok {
			procs = append(procs, *proc)
		}
	}

	return procs, nil
}

// ExecuteProcedure executes a stored procedure by name with the given named parameters.
// The procedure name is validated against a safe-identifier regex before execution.
// Parameters are passed as named SQL parameters to prevent SQL injection.
// Example: ExecuteProcedure(ctx, "sp_CiroHesapla", map[string]string{"StartDate": "2024-01-01", "EndDate": "2024-12-31"})
func (m *MSSQLAdapter) ExecuteProcedure(ctx context.Context, name string, params map[string]string) (*core.QueryResult, error) {
	if !safeProcName.MatchString(name) {
		return nil, fmt.Errorf("invalid procedure name %q: only letters, digits, underscores, # and @ are allowed", name)
	}

	// Build: EXEC sp_Name @Param1=@Param1, @Param2=@Param2
	// with sql.Named(...) for each parameter value (prevents injection).
	namedArgs := make([]any, 0, len(params))
	paramParts := make([]string, 0, len(params))

	for rawName, value := range params {
		// Strip a leading @ if the caller included it
		cleanName := strings.TrimPrefix(rawName, "@")
		if !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(cleanName) {
			return nil, fmt.Errorf("invalid parameter name %q", rawName)
		}
		namedArgs = append(namedArgs, sql.Named(cleanName, value))
		paramParts = append(paramParts, fmt.Sprintf("@%s=@%s", cleanName, cleanName))
	}

	var execSQL string
	if len(paramParts) > 0 {
		execSQL = fmt.Sprintf("EXEC %s %s", name, strings.Join(paramParts, ", "))
	} else {
		execSQL = fmt.Sprintf("EXEC %s", name)
	}

	rows, err := m.db.QueryContext(ctx, execSQL, namedArgs...)
	if err != nil {
		return nil, fmt.Errorf("procedure execution failed: %w", err)
	}
	defer rows.Close()

	return m.parseRows(rows)
}

// executeQueryReadUncommitted runs the query inside a READ UNCOMMITTED transaction.// This is equivalent to appending WITH (NOLOCK) to every table reference.
// The transaction is always rolled back afterwards since we only perform reads.
func (m *MSSQLAdapter) executeQueryReadUncommitted(ctx context.Context, query string, args ...any) (*core.QueryResult, error) {
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadUncommitted})
	if err != nil {
		return nil, fmt.Errorf("failed to begin READ UNCOMMITTED transaction: %w", err)
	}
	// Always roll back — we never modify data, rollback is a no-op for pure reads.
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return m.parseRows(rows)
}

func (m *MSSQLAdapter) parseRows(rows *sql.Rows) (*core.QueryResult, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	count := len(columns)
	values := make([]interface{}, count)
	valuePtrs := make([]interface{}, count)

	var finalRows []map[string]any

	for rows.Next() {
		for i := range columns {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}

		rowMap := make(map[string]any)
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			if m.normalizeTurkish {
				val = turkish.FixResultValue(val)
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
