// Package migrations owns the legacy schema this platform was built on top of.
//
// 001_initial.sql is NOT part of the numbered, checksum-immutable histories in
// services/elitea-main/migrations — it is the legacy-era schema those histories
// assume already exists. shared/0030 declares a foreign key to centry.project,
// and most of the centry tables are created here rather than by the Go chain,
// so an empty database cannot be migrated until this has been applied.
//
// It is embedded so the migration binary can apply it itself. Before that, the
// only things that could were the compose stack (a bind mount) and the E2E
// seeder (a psql invocation), which is why a Kubernetes install had no way to
// reach a migrated database without a human running psql.
//
// The file stays at this path rather than moving next to the histories: the
// compose stack, the E2E seeder and several integration tests all read it here,
// and it is deliberately NOT in the corpus that embed.go over there declares
// immutable.
package migrations

import _ "embed"

// Initial is the legacy-era schema, applied once to a database that does not
// already carry it. Exported as a string rather than an fs.FS because there is
// exactly one file and its contents are applied whole.
//
//go:embed 001_initial.sql
var Initial string
