// Package migrations embeds reviewed PostgreSQL schema migrations.
package migrations

import "embed"

// Files contains all paired up/down SQL migration files.
//
//go:embed *.sql
var Files embed.FS
