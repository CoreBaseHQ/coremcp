// Package adapter provides a factory for creating database adapters.
package adapter

import (
	"fmt"

	"github.com/corebasehq/coremcp/pkg/adapter/dummy"
	"github.com/corebasehq/coremcp/pkg/adapter/graphql"
	"github.com/corebasehq/coremcp/pkg/adapter/mssql"
	"github.com/corebasehq/coremcp/pkg/adapter/rest"
	"github.com/corebasehq/coremcp/pkg/core"
)

// NewSource creates a new database adapter based on the specified type.
// Supported types: "dummy", "mssql", "rest", "graphql", "firebird" (coming soon).
// noLock enables READ UNCOMMITTED isolation for MSSQL sources (equivalent to WITH (NOLOCK)).
// normalizeTurkish enables Turkish character normalization middleware for legacy Turkish_CI_AS databases.
// Returns an error if the database type is unsupported or initialization fails.
func NewSource(dbType string, dsn string, noLock bool, normalizeTurkish bool) (core.Source, error) {
	switch dbType {
	case "dummy":
		return dummy.New(dsn)
	case "mssql":
		return mssql.New(dsn, noLock, normalizeTurkish)
	case "rest":
		return rest.New(dsn)
	case "graphql":
		return graphql.New(dsn)
	case "firebird":
		return nil, fmt.Errorf("Firebird is not implemented yet")
	default:
		return nil, fmt.Errorf("unsupported DB type: %s", dbType)
	}
}
