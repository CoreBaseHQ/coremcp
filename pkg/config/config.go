// Package config provides configuration management for CoreMCP.
// It supports YAML configuration files and environment variables.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Config represents the complete CoreMCP configuration.
type Config struct {
	Server      ServerConfig       `mapstructure:"server"`
	Logging     LoggingConfig      `mapstructure:"logging"`
	Sources     []SourceConfig     `mapstructure:"sources"`
	CustomTools []CustomToolConfig `mapstructure:"custom_tools"`
	Security    SecurityConfig     `mapstructure:"security"`
}

// SecurityConfig holds security-related configuration.
type SecurityConfig struct {
	MaxRowLimit      int              `mapstructure:"max_row_limit"`      // Maximum rows to return (default: 1000)
	EnablePIIMasking bool             `mapstructure:"enable_pii_masking"` // Enable PII data masking
	PIIPatterns      []PIIMaskPattern `mapstructure:"pii_patterns"`       // PII patterns to mask
	AllowedKeywords  []string         `mapstructure:"allowed_keywords"`   // SQL keywords to allow beyond SELECT/WITH
	BlockedKeywords  []string         `mapstructure:"blocked_keywords"`   // Additional SQL keywords to block
}

// PIIMaskPattern defines a pattern for masking PII data.
type PIIMaskPattern struct {
	Name        string `mapstructure:"name"`        // Pattern name (e.g., "credit_card")
	Pattern     string `mapstructure:"pattern"`     // Regex pattern
	Replacement string `mapstructure:"replacement"` // Replacement string (default: "***")
	Enabled     bool   `mapstructure:"enabled"`     // Whether this pattern is active
}

// ServerConfig holds server-related configuration.
type ServerConfig struct {
	Name      string `mapstructure:"name"`
	Version   string `mapstructure:"version"`
	Transport string `mapstructure:"transport"`
	Port      int    `mapstructure:"port"`
}

type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
}

type SourceConfig struct {
	Name             string `mapstructure:"name"`
	Type             string `mapstructure:"type"` // mssql, firebird, postgres, rest, graphql
	DSN              string `mapstructure:"dsn"`  // Data Source Name (Connection String)
	ReadOnly         *bool  `mapstructure:"readonly"`
	NoLock           bool   `mapstructure:"no_lock"`           // MSSQL: Use READ UNCOMMITTED isolation (equivalent to WITH (NOLOCK))
	NormalizeTurkish bool   `mapstructure:"normalize_turkish"` // Normalize Turkish chars in SQL literals (for legacy Turkish_CI_AS databases)
	// REST/GraphQL specific fields
	BaseURL string            `mapstructure:"base_url"` // Base URL for REST/GraphQL APIs
	APIKey  string            `mapstructure:"api_key"`  // API key (will be encrypted at rest)
	SpecURL string            `mapstructure:"spec_url"` // OpenAPI spec URL for REST
	Headers map[string]string `mapstructure:"headers"`  // Custom headers for REST/GraphQL
	Timeout int               `mapstructure:"timeout"`  // Request timeout in seconds (default: 30)
}

// IsReadOnly returns true if this source is read-only (defaults to true when not explicitly set).
func (s SourceConfig) IsReadOnly() bool {
	if s.ReadOnly == nil {
		return true
	}
	return *s.ReadOnly
}

// BuildDSN builds the connection string (DSN) for this source.
// For REST/GraphQL sources, it constructs the DSN from BaseURL, APIKey, and other fields.
// For database sources, it returns the configured DSN.
func (s SourceConfig) BuildDSN() string {
	// If DSN is already provided, use it (backward compatibility)
	if s.DSN != "" {
		return s.DSN
	}

	// Build DSN for REST/GraphQL sources
	switch s.Type {
	case "rest":
		return s.buildRESTDSN()
	case "graphql":
		return s.buildGraphQLDSN()
	default:
		return s.DSN
	}
}

func (s SourceConfig) buildRESTDSN() string {
	if s.BaseURL == "" {
		return ""
	}

	u, err := url.Parse(s.BaseURL)
	if err != nil {
		return ""
	}

	// Convert to rest:// scheme
	u.Scheme = "rest"

	q := u.Query()

	// Add API key if provided
	if s.APIKey != "" {
		q.Set("apiKey", s.APIKey)
	}

	// Add spec URL if provided
	if s.SpecURL != "" {
		q.Set("specURL", s.SpecURL)
	}

	// Add custom headers
	for k, v := range s.Headers {
		q.Set("header_"+k, v)
	}

	// Add timeout if specified
	if s.Timeout > 0 {
		q.Set("timeout", fmt.Sprintf("%d", s.Timeout))
	}

	u.RawQuery = q.Encode()
	return u.String()
}

func (s SourceConfig) buildGraphQLDSN() string {
	if s.BaseURL == "" {
		return ""
	}

	u, err := url.Parse(s.BaseURL)
	if err != nil {
		return ""
	}

	// Convert to graphql:// scheme
	u.Scheme = "graphql"

	q := u.Query()

	// Add API key if provided
	if s.APIKey != "" {
		q.Set("apiKey", s.APIKey)
	}

	// Add custom headers
	for k, v := range s.Headers {
		q.Set("header_"+k, v)
	}

	// Add timeout if specified
	if s.Timeout > 0 {
		q.Set("timeout", fmt.Sprintf("%d", s.Timeout))
	}

	u.RawQuery = q.Encode()
	return u.String()
}

// CustomToolConfig defines a custom MCP tool with a predefined query.
type CustomToolConfig struct {
	Name        string          `mapstructure:"name"`        // Tool name (e.g., "get_daily_sales")
	Description string          `mapstructure:"description"` // Tool description for AI
	Source      string          `mapstructure:"source"`      // Which source to run against
	Query       string          `mapstructure:"query"`       // SQL query template
	Parameters  []ToolParameter `mapstructure:"parameters"`  // Optional parameters
}

// ToolParameter defines a parameter for a custom tool.
type ToolParameter struct {
	Name        string `mapstructure:"name"`
	Description string `mapstructure:"description"`
	Required    bool   `mapstructure:"required"`
	Default     string `mapstructure:"default"`
	// Type constrains the allowed values for this parameter.
	// Recognised values: "string" (default), "integer", "number", "date", "identifier".
	// Using a precise type is strongly recommended to prevent SQL injection when the
	// parameter is interpolated directly into the query template without quotes.
	Type string `mapstructure:"type"`
}

// LoadConfig loads the CoreMCP configuration from a YAML file.
// It supports environment variables with COREMCP_ prefix and provides sensible defaults.
// The path parameter can be a directory or a full file path.
func LoadConfig(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")

	// Determine whether `path` is a full file path or a directory.
	// When a full file path is given (e.g. /home/yiit/coremcp/coremcp.yaml),
	// set the config file directly so Viper doesn't rely on CWD resolution.
	if path != "" && path != "coremcp.yaml" {
		abs, err := filepath.Abs(path)
		if err == nil {
			info, statErr := os.Stat(abs)
			if statErr == nil && !info.IsDir() {
				// Full file path — tell Viper exactly which file to use.
				v.SetConfigFile(abs)
			} else {
				// It's a directory — add it as a search path.
				v.SetConfigName("coremcp")
				v.AddConfigPath(abs)
			}
		}
	} else {
		// Default: search for coremcp.yaml relative to CWD,
		// then the directory of the running binary.
		v.SetConfigName("coremcp")
		v.AddConfigPath(".")
		if exe, err := os.Executable(); err == nil {
			v.AddConfigPath(filepath.Dir(exe))
		}
	}

	// Environment Variables
	v.SetEnvPrefix("COREMCP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Fallback
	v.SetDefault("server.transport", "stdio")
	v.SetDefault("server.port", 8080)
	v.SetDefault("logging.level", "info")
	v.SetDefault("security.max_row_limit", 1000)
	v.SetDefault("security.enable_pii_masking", false)

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			fmt.Fprintf(os.Stderr, "Warning: Config file not found, using defaults.\n")
		} else {
			return nil, fmt.Errorf("failed to read config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// If no sources defined, add a default dummy source
	if len(cfg.Sources) == 0 {
		cfg.Sources = []SourceConfig{
			{
				Name: "dummy",
				Type: "dummy",
				DSN:  "dummy://default",
				// ReadOnly defaults to true via IsReadOnly()
			},
		}
	}

	return &cfg, nil
}
