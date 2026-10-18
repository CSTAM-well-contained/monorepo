// Package migrations holds the PostgreSQL schema as goose SQL files, embedded into cpd.
package migrations

import "embed"

// FS contains every *.sql migration, applied in file-name order.
//
//go:embed *.sql
var FS embed.FS
