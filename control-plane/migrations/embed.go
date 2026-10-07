// Package migrations embeds the SQL migrations: the Postgres ones in this directory and
// their SQLite twins in sqlite/.
package migrations

import "embed"

// FS holds the Postgres migrations.
//
//go:embed *.sql
var FS embed.FS

// SQLiteFS holds the SQLite migrations, under the directory "sqlite".
//
//go:embed sqlite/*.sql
var SQLiteFS embed.FS
