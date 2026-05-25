// Package security provides query sanitization and data masking functionality.
package security

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/xwb1989/sqlparser"
)

// QueryValidator validates SQL queries for security purposes.
type QueryValidator struct {
	allowedKeywords []string
	blockedKeywords []string
	blockedRegex    []blockedPattern
}

type blockedPattern struct {
	word string
	re   *regexp.Regexp
}

var defaultBlockedKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "TRUNCATE",
	"CREATE", "GRANT", "REVOKE", "EXEC", "EXECUTE", "MERGE",
	"REPLACE", "RENAME", "CALL", "LOAD", "COPY",
}

// NewQueryValidator creates a new query validator with custom allowed/blocked keywords.
func NewQueryValidator(allowedKeywords, blockedKeywords []string) *QueryValidator {
	allBlocked := make([]string, 0, len(defaultBlockedKeywords)+len(blockedKeywords))
	allBlocked = append(allBlocked, defaultBlockedKeywords...)
	allBlocked = append(allBlocked, blockedKeywords...)

	compiled := make([]blockedPattern, 0, len(allBlocked))
	for _, word := range allBlocked {
		pattern := fmt.Sprintf(`(?i)\b%s\b`, regexp.QuoteMeta(word))
		compiled = append(compiled, blockedPattern{word: word, re: regexp.MustCompile(pattern)})
	}

	return &QueryValidator{
		allowedKeywords: allowedKeywords,
		blockedKeywords: blockedKeywords,
		blockedRegex:    compiled,
	}
}

// ValidateQuery validates if a SQL query is safe to execute.
// Uses sqlparser for AST-based analysis to detect dangerous operations.
func (qv *QueryValidator) ValidateQuery(query string) error {
	// First, try to parse the query with sqlparser
	stmt, err := sqlparser.Parse(query)
	if err != nil {
		// If parsing fails, fall back to regex-based validation
		return qv.validateWithRegex(query)
	}

	// Check statement type
	switch s := stmt.(type) {
	case *sqlparser.Select:
		// SELECT is allowed
		return qv.validateSelectStatement(s)
	case *sqlparser.Union:
		// UNION is allowed (it's just multiple SELECTs)
		return nil
	case *sqlparser.Insert, *sqlparser.Update, *sqlparser.Delete:
		return fmt.Errorf("write operations (INSERT/UPDATE/DELETE) are not allowed")
	case *sqlparser.DDL:
		return fmt.Errorf("DDL operations (CREATE/ALTER/DROP/TRUNCATE) are not allowed")
	case *sqlparser.OtherAdmin:
		return fmt.Errorf("administrative operations are not allowed")
	default:
		return fmt.Errorf("unsupported query type: %T", stmt)
	}
}

// validateSelectStatement performs deeper validation on SELECT statements.
func (qv *QueryValidator) validateSelectStatement(_ *sqlparser.Select) error {
	// Check for subqueries that might contain dangerous operations
	// This is a simplified check - sqlparser already ensures SELECT-only in subqueries

	// Additional custom validations can be added here
	// For example, checking for specific function calls, etc.

	return nil
}

// validateWithRegex performs regex-based validation as a fallback.
func (qv *QueryValidator) validateWithRegex(query string) error {
	q := strings.TrimSpace(strings.ToUpper(query))

	// Allow only SELECT and WITH (CTE) statements
	if !strings.HasPrefix(q, "SELECT") && !strings.HasPrefix(q, "WITH") {
		return fmt.Errorf("only SELECT and WITH queries are allowed")
	}

	for _, blocked := range qv.blockedRegex {
		// Check for whole word matches to avoid false positives
		if blocked.re.MatchString(query) {
			return fmt.Errorf("blocked keyword detected: %s", blocked.word)
		}
	}

	return nil
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

// AddRowLimit adds a LIMIT clause to a query if it doesn't already have one.
func (qm *QueryModifier) AddRowLimit(query string) (string, error) {
	// xwb1989/sqlparser is MySQL-dialect; round-tripping a T-SQL query through
	// sqlparser.String can mangle MSSQL-specific syntax (brackets, NOLOCK hints,
	// N'...' literals, OUTPUT clauses). When the query looks like T-SQL, skip the
	// AST path entirely and use the regex-based rewriter, which only edits the
	// LIMIT/TOP/FETCH numeric tokens and leaves everything else intact.
	if looksLikeMSSQL(strings.ToUpper(query)) {
		return qm.addRowLimitSimple(query), nil
	}

	// Parse the query
	stmt, err := sqlparser.Parse(query)
	if err != nil {
		// Fallback to simple string append if parsing fails
		return qm.addRowLimitSimple(query), nil
	}

	switch s := stmt.(type) {
	case *sqlparser.Select:
		// If the query already has a LIMIT that is within the allowed maximum, keep it.
		// If the existing limit exceeds the maximum, override it to prevent overload.
		if s.Limit != nil && s.Limit.Rowcount != nil {
			if limVal, ok := s.Limit.Rowcount.(*sqlparser.SQLVal); ok && limVal.Type == sqlparser.IntVal {
				existing, err := strconv.Atoi(string(limVal.Val))
				if err == nil && existing <= qm.maxRowLimit {
					return query, nil
				}
			}
		}

		// Add or override LIMIT clause, preserving any existing OFFSET
		var existingOffset sqlparser.Expr
		if s.Limit != nil {
			existingOffset = s.Limit.Offset
		}

		s.Limit = &sqlparser.Limit{
			Offset:   existingOffset,
			Rowcount: sqlparser.NewIntVal([]byte(fmt.Sprintf("%d", qm.maxRowLimit))),
		}

		return sqlparser.String(s), nil

	case *sqlparser.Union:
		// Appending "LIMIT N" directly to a UNION would only cap the trailing
		// SELECT after the MSSQL adapter rewrites LIMIT→TOP. Wrap the whole
		// UNION in a subquery so the cap applies to the combined result set
		// regardless of dialect.
		return fmt.Sprintf("SELECT * FROM (%s) AS _capped LIMIT %d", query, qm.maxRowLimit), nil

	default:
		return query, nil
	}
}

// limitRe matches a LIMIT clause with its numeric value, optionally followed by an OFFSET.
// Used by addRowLimitSimple to safely replace only the row-count token.
var (
	limitRe           = regexp.MustCompile(`(?i)\bLIMIT\s+(\d+)`)
	mssqlSelectRe     = regexp.MustCompile(`(?i)\bSELECT\s+(DISTINCT\s+)?`)
	mssqlTopRe        = regexp.MustCompile(`(?i)\bSELECT\s+(DISTINCT\s+)?TOP\s+\d+\b`)
	mssqlFetchRe      = regexp.MustCompile(`(?i)\bFETCH\s+NEXT\s+\d+\s+ROWS?\s+ONLY\b`)
	mssqlTopValueRe   = regexp.MustCompile(`(?i)(\bTOP\s+)(\d+)\b`)
	mssqlFetchValueRe = regexp.MustCompile(`(?i)(\bFETCH\s+NEXT\s+)(\d+)(\s+ROWS?\s+ONLY\b)`)
)

// addRowLimitSimple adds or overrides the LIMIT clause using simple string
// manipulation. It is used as a fallback when the SQL parser cannot parse the
// query (e.g., dialect-specific syntax). Any existing LIMIT that exceeds
// maxRowLimit is replaced so the cap is enforced consistently.
// OFFSET and any other clauses that follow the LIMIT value are preserved.
func (qm *QueryModifier) addRowLimitSimple(query string) string {
	q := strings.TrimSpace(query)
	upper := strings.ToUpper(q)

	if m := limitRe.FindStringSubmatchIndex(q); m != nil {
		// m[2]:m[3] is the capture group holding the numeric value.
		existing, err := strconv.Atoi(q[m[2]:m[3]])
		if err == nil && existing <= qm.maxRowLimit {
			// Existing limit is within the allowed maximum — keep query unchanged.
			return query
		}
		// Replace only the numeric token; preserve everything before and after.
		return q[:m[2]] + strconv.Itoa(qm.maxRowLimit) + q[m[3]:]
	}

	if looksLikeMSSQL(upper) {
		return qm.addRowLimitMSSQL(q)
	}

	// No LIMIT found — append one.
	return fmt.Sprintf("%s LIMIT %d", q, qm.maxRowLimit)
}

// looksLikeMSSQL is a best-effort heuristic that flags queries containing
// T-SQL-specific syntax (NOLOCK hint, TOP/FETCH NEXT, bracketed identifiers,
// or common MSSQL types/functions). False positives are harmless because they
// only steer the row-cap rewriter toward the regex-based path that preserves
// the original query verbatim.
func looksLikeMSSQL(upperQuery string) bool {
	if strings.Contains(upperQuery, "WITH (NOLOCK)") {
		return true
	}
	if mssqlFetchRe.MatchString(upperQuery) {
		return true
	}
	if strings.Contains(upperQuery, "TOP ") {
		return true
	}
	if strings.Contains(upperQuery, "NVARCHAR") || strings.Contains(upperQuery, "GETDATE()") {
		return true
	}
	if strings.Contains(upperQuery, "[") && strings.Contains(upperQuery, "]") {
		return true
	}
	return false
}

func (qm *QueryModifier) addRowLimitMSSQL(query string) string {
	if mssqlTopRe.MatchString(query) {
		return mssqlTopValueRe.ReplaceAllStringFunc(query, func(match string) string {
			parts := mssqlTopValueRe.FindStringSubmatch(match)
			if len(parts) != 3 {
				return match
			}
			existing, err := strconv.Atoi(parts[2])
			if err != nil || existing <= qm.maxRowLimit {
				return match
			}
			return fmt.Sprintf("%s%d", parts[1], qm.maxRowLimit)
		})
	}
	if mssqlFetchRe.MatchString(query) {
		return mssqlFetchValueRe.ReplaceAllStringFunc(query, func(match string) string {
			parts := mssqlFetchValueRe.FindStringSubmatch(match)
			if len(parts) != 4 {
				return match
			}
			existing, err := strconv.Atoi(parts[2])
			if err != nil || existing <= qm.maxRowLimit {
				return match
			}
			return fmt.Sprintf("%s%d%s", parts[1], qm.maxRowLimit, parts[3])
		})
	}

	loc := mssqlSelectRe.FindStringSubmatchIndex(query)
	if loc == nil {
		return query
	}

	insertAt := loc[1]
	if len(loc) >= 4 && loc[2] != -1 {
		insertAt = loc[3]
	}

	return query[:insertAt] + fmt.Sprintf("TOP %d ", qm.maxRowLimit) + query[insertAt:]
}
