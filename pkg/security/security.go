// Package security provides query sanitization and data masking functionality.
package security

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// QueryValidator validates SQL queries for security purposes.
//
// Design note — why we do not use a third-party SQL parser:
//
// Previously this package used xwb1989/sqlparser (MySQL dialect, unmaintained
// since 2018) with a regex denylist fallback. The fallback was bypassable via
// comment/whitespace tricks (e.g. "SELECT 1; EX/**/EC xp_cmdshell …" — the
// regex would not match "EX/**/EC" but SQL Server treats /**/ as whitespace
// and executes the statement). Switching to vitess or cockroachdb parsers
// only moves the problem: every dialect-aware parser has corner cases where
// it cannot parse a legitimate target-DB query, and any "fall through to
// regex" relaxation re-introduces the same bypass class.
//
// We do not actually need an AST. The security goal is to classify the
// statement type (SELECT/WITH allowed, everything else rejected) and to
// reject multi-statement payloads. Both checks are reliable on a properly
// stripped query (no comments, no string literals). This file does that with
// a small custom tokeniser — easier to audit and free of parser-dialect
// gotchas.
//
// The allowedKeywords / blockedKeywords slices are retained on the struct
// for API + config-payload backwards compatibility (the cloud control plane
// and existing coremcp.yaml files still reference them) but they are no
// longer consulted by the validator.
type QueryValidator struct {
	allowedKeywords []string //nolint:unused // retained for API compatibility; see type doc
	blockedKeywords []string //nolint:unused // retained for API compatibility; see type doc
}

// NewQueryValidator creates a new query validator.
//
// The allowedKeywords / blockedKeywords parameters are accepted but ignored —
// see the QueryValidator type doc for why. The signature is preserved so the
// MCP server and the WebSocket config_sync code path do not have to change.
func NewQueryValidator(allowedKeywords, blockedKeywords []string) *QueryValidator {
	return &QueryValidator{
		allowedKeywords: allowedKeywords,
		blockedKeywords: blockedKeywords,
	}
}

// forbiddenInBodyRe matches tokens that must never appear in the body of a
// SELECT or WITH statement, even when the leading-keyword classifier has
// accepted the shape. Each of these turns a SELECT-shaped statement into a
// write or exfiltration vector:
//
//   - INTO            "SELECT … INTO new_table FROM …" creates a table (MSSQL,
//                     PostgreSQL); "SELECT … INTO OUTFILE/DUMPFILE …" writes
//                     to the server filesystem (MySQL); "SELECT … INTO @var"
//                     mutates T-SQL session state.
//   - OPENROWSET      T-SQL ad-hoc distributed query — can pull data from any
//                     remote endpoint reachable from the DB server. Classic
//                     exfiltration vector.
//   - OPENQUERY       T-SQL distributed query against a linked server.
//   - OPENDATASOURCE  T-SQL ad-hoc linked-server connection.
//
// The scan runs on a copy of the query with comments and string literals
// already stripped, so single-quoted decoys and "EX/**/EC" tricks cannot
// hide the token. The only residual false-positive surface is a column or
// table whose unquoted name exactly matches one of these reserved-ish
// words — extremely rare in real schemas, and the error message is explicit
// so the user can rename or escape.
var forbiddenInBodyRe = regexp.MustCompile(`(?i)\b(INTO|OPENROWSET|OPENQUERY|OPENDATASOURCE)\b`)

// ValidateQuery decides whether a SQL query is safe to forward to a source.
//
// Posture: fail-closed. Four layers:
//  1. Reject multi-statement payloads — any ';' outside strings/comments,
//     other than a single trailing one, is fatal. This catches stacked-query
//     attacks regardless of dialect.
//  2. Classify the leading statement keyword (SELECT / WITH allowed; every
//     other recognised keyword is rejected with a specific error; unknown
//     leading tokens are rejected generically).
//  3. The leading keyword is read from the query AFTER stripping comments
//     and string literals, so payloads like "/* SELECT */ DROP TABLE x"
//     or "SELECT 'safe'; DELETE FROM users" cannot lie about their shape.
//  4. For SELECT/WITH bodies, scan the stripped text for write/exfiltration
//     tokens that cannot legitimately appear in a read-only query
//     (SELECT…INTO, OPENROWSET, OPENQUERY, OPENDATASOURCE).
//
// This validator does NOT introspect CTE bodies for DML keywords. For
// PostgreSQL, a CTE may legally contain "DELETE … RETURNING" — the line of
// defence there is the DB user's permission set (the README requires a
// SELECT-only user, and the cloud-direct adapter additionally pins the
// session to default_transaction_read_only). MSSQL CTEs are SELECT-only at
// the grammar level, so the primary target is not affected.
func (qv *QueryValidator) ValidateQuery(query string) error {
	if hasMultipleStatements(query) {
		return fmt.Errorf("multi-statement queries are not allowed")
	}

	kw := firstKeyword(query)
	if kw == "" {
		return fmt.Errorf("query rejected: no SQL statement found")
	}

	switch kw {
	case "SELECT", "WITH":
		// Statement shape is OK — but scan the body for write/exfil tokens
		// that turn a SELECT-shaped statement into something dangerous.
		if m := forbiddenInBodyRe.FindString(stripCommentsAndStrings(query)); m != "" {
			return fmt.Errorf("query rejected: forbidden token %q in SELECT/WITH body (SELECT...INTO, OPENROWSET-family and similar are not allowed)", strings.ToUpper(m))
		}
		return nil
	case "INSERT", "UPDATE", "DELETE", "MERGE", "REPLACE", "UPSERT":
		return fmt.Errorf("write operations are not allowed (%s)", kw)
	case "DROP", "ALTER", "CREATE", "TRUNCATE", "RENAME", "COMMENT":
		return fmt.Errorf("DDL operations are not allowed (%s)", kw)
	case "EXEC", "EXECUTE", "CALL":
		return fmt.Errorf("procedure execution must use the execute_procedure tool, not inline %s", kw)
	case "GRANT", "REVOKE":
		return fmt.Errorf("permission statements are not allowed (%s)", kw)
	case "USE", "SET", "DECLARE", "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT":
		return fmt.Errorf("session/control statements are not allowed (%s)", kw)
	case "LOAD", "COPY", "BULK", "BACKUP", "RESTORE", "DBCC", "SHUTDOWN", "KILL":
		return fmt.Errorf("data-movement / admin statements are not allowed (%s)", kw)
	default:
		return fmt.Errorf("query rejected: only SELECT and WITH (CTE) statements are accepted (got %q)", kw)
	}
}

// firstKeyword returns the first SQL identifier (uppercased) at the start of
// the query — after stripping comments, string literals, leading whitespace,
// and leading opening parentheses. Returns "" if the query has no
// identifier-shaped token at the start.
//
// Leading parens are skipped so wrappers like "(SELECT …)" or
// "((SELECT a) UNION (SELECT b))" classify correctly as SELECT-shaped.
func firstKeyword(query string) string {
	s := stripCommentsAndStrings(query)
	i := 0
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(' {
			i++
			continue
		}
		break
	}
	if i >= len(s) {
		return ""
	}
	j := i
	for j < len(s) {
		c := s[j]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			j++
			continue
		}
		break
	}
	if j == i {
		return ""
	}
	return strings.ToUpper(s[i:j])
}

// hasMultipleStatements reports whether `query` contains a statement separator
// (";") outside of string literals or comments. A single trailing semicolon
// is tolerated since it is a common, harmless idiom.
func hasMultipleStatements(query string) bool {
	stripped := stripCommentsAndStrings(query)
	stripped = strings.TrimRight(stripped, "; \t\n\r")
	return strings.ContainsRune(stripped, ';')
}

// stripCommentsAndStrings removes /* */ block comments, -- line comments, and
// single-quoted string literals (including the SQL standard '' escape) so the
// remaining text can be scanned for structural tokens like ';' without being
// fooled by content hidden inside strings or comments.
//
// Replacements are written as a single space so adjacent tokens never fuse —
// "EX/**/EC" becomes "EX EC", not "EXEC".
func stripCommentsAndStrings(query string) string {
	var b strings.Builder
	b.Grow(len(query))
	i, n := 0, len(query)
	for i < n {
		c := query[i]

		// /* ... */ block comment
		if c == '/' && i+1 < n && query[i+1] == '*' {
			j := strings.Index(query[i+2:], "*/")
			if j < 0 {
				b.WriteByte(' ')
				return b.String()
			}
			i += 2 + j + 2
			b.WriteByte(' ')
			continue
		}

		// -- line comment
		if c == '-' && i+1 < n && query[i+1] == '-' {
			j := strings.IndexByte(query[i+2:], '\n')
			if j < 0 {
				b.WriteByte(' ')
				return b.String()
			}
			i += 2 + j + 1
			b.WriteByte(' ')
			continue
		}

		// 'string literal' with '' escape
		if c == '\'' {
			i++
			for i < n {
				if query[i] == '\'' {
					if i+1 < n && query[i+1] == '\'' {
						i += 2 // escaped single quote
						continue
					}
					i++ // closing quote
					break
				}
				i++
			}
			b.WriteByte(' ')
			continue
		}

		b.WriteByte(c)
		i++
	}
	return b.String()
}

// PIIMasker masks personally identifiable information in query results.
type PIIMasker struct {
	patterns     []*regexp.Regexp
	replacements []string
	enabled      bool
}

// MaskPattern represents a PII masking pattern.
type MaskPattern struct {
	Name        string
	Pattern     string
	Replacement string
	Enabled     bool
}

// NewPIIMasker creates a new PII masker with configured patterns.
func NewPIIMasker(patterns []MaskPattern, enabled bool) (*PIIMasker, error) {
	if !enabled {
		return &PIIMasker{enabled: false}, nil
	}

	masker := &PIIMasker{
		patterns:     make([]*regexp.Regexp, 0),
		replacements: make([]string, 0),
		enabled:      true,
	}

	for _, p := range patterns {
		if !p.Enabled {
			continue
		}

		regex, err := regexp.Compile(p.Pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern for %s: %w", p.Name, err)
		}

		masker.patterns = append(masker.patterns, regex)

		replacement := p.Replacement
		if replacement == "" {
			replacement = "***MASKED***"
		}
		masker.replacements = append(masker.replacements, replacement)
	}

	return masker, nil
}

// MaskData masks PII data in a string.
func (pm *PIIMasker) MaskData(data string) string {
	if !pm.enabled {
		return data
	}

	result := data
	for i, pattern := range pm.patterns {
		result = pattern.ReplaceAllString(result, pm.replacements[i])
	}

	return result
}

// MaskValue masks PII in any value (handles different types).
func (pm *PIIMasker) MaskValue(value interface{}) interface{} {
	if !pm.enabled {
		return value
	}

	switch v := value.(type) {
	case string:
		return pm.MaskData(v)
	case []byte:
		return pm.MaskData(string(v))
	default:
		return value
	}
}

// DefaultPIIPatterns returns commonly used PII masking patterns.
func DefaultPIIPatterns() []MaskPattern {
	return []MaskPattern{
		{
			Name:        "credit_card",
			Pattern:     `\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b`,
			Replacement: "****-****-****-****",
			Enabled:     true,
		},
		{
			Name:        "ssn_us",
			Pattern:     `\b\d{3}-\d{2}-\d{4}\b`,
			Replacement: "***-**-****",
			Enabled:     true,
		},
		{
			Name:        "email",
			Pattern:     `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b`,
			Replacement: "***@***.***",
			Enabled:     true,
		},
		{
			Name:        "phone_us",
			Pattern:     `\b\d{3}[-.\s]?\d{3}[-.\s]?\d{4}\b`,
			Replacement: "***-***-****",
			Enabled:     true,
		},
		{
			Name:        "turkish_id",
			Pattern:     `\b[1-9]\d{10}\b`,
			Replacement: "***********",
			Enabled:     true,
		},
		{
			Name:        "iban",
			Pattern:     `\b[A-Z]{2}\d{2}[A-Z0-9]{1,30}\b`,
			Replacement: "********************",
			Enabled:     true,
		},
	}
}

// QueryModifier modifies queries to add safety constraints.
type QueryModifier struct {
	maxRowLimit int
}

// NewQueryModifier creates a new query modifier.
func NewQueryModifier(maxRowLimit int) *QueryModifier {
	if maxRowLimit <= 0 {
		maxRowLimit = 1000 // Default limit
	}
	return &QueryModifier{
		maxRowLimit: maxRowLimit,
	}
}

// trailingLimitRe matches a "LIMIT N" (optionally followed by "OFFSET M") at
// the very end of the query. Anchoring on $ means a LIMIT clause inside a
// subquery is not mistaken for the outer cap — those queries get a fresh
// outer LIMIT appended.
var trailingLimitRe = regexp.MustCompile(`(?i)\bLIMIT\s+(\d+)(?:\s+OFFSET\s+\d+)?\s*$`)

// AddRowLimit ensures the query carries a top-level "LIMIT N" no larger than
// maxRowLimit.
//
// The modifier is dialect-agnostic and uses string manipulation only — it
// does not pull in a SQL parser. Behaviour:
//   - Trailing whitespace and a single trailing ';' are tolerated and stripped.
//   - If a trailing "LIMIT N [OFFSET M]" already exists with N <= maxRowLimit,
//     the query is returned unchanged.
//   - If N exceeds the cap, only the numeric token is replaced; OFFSET and
//     anything before LIMIT are preserved.
//   - Otherwise " LIMIT maxRowLimit" is appended.
//
// On MSSQL the adapter strips this LIMIT and rewrites it as SELECT TOP N
// (see pkg/adapter/mssql/mssql.go adaptQueryForVersion). Subquery LIMITs
// inside the query body are left alone — only the outermost cap is managed.
func (qm *QueryModifier) AddRowLimit(query string) (string, error) {
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	q = strings.TrimSpace(q)

	if m := trailingLimitRe.FindStringSubmatchIndex(q); m != nil {
		existing, err := strconv.Atoi(q[m[2]:m[3]])
		if err == nil && existing <= qm.maxRowLimit {
			// Existing trailing LIMIT is within the cap — keep query unchanged
			// (preserving any trailing semicolon the caller had).
			return query, nil
		}
		// Replace only the numeric token; everything before and after
		// (OFFSET clause, trailing whitespace) is preserved.
		return q[:m[2]] + strconv.Itoa(qm.maxRowLimit) + q[m[3]:], nil
	}

	return fmt.Sprintf("%s LIMIT %d", q, qm.maxRowLimit), nil
}
