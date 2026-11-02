package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

// Schema is the migration state of a database as this build sees it.
type Schema struct {
	Version int64   `json:"version"`           // highest migration recorded as applied
	Latest  int64   `json:"latest"`            // highest migration this build has
	Pending []int64 `json:"pending,omitempty"` // migrations of this build not recorded as applied
}

// Current reports whether every migration of this build is recorded as
// applied.
func (s Schema) Current() bool { return len(s.Pending) == 0 }

// ReadSchema compares goose's migration record with this build's
// migrations. It only reads: a missing record table means nothing has been
// applied (goose's own status calls would create the table).
func ReadSchema(ctx context.Context, db *pgxpool.Pool) (Schema, error) {
	versions, err := sourceVersions()
	if err != nil {
		return Schema{}, err
	}
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&exists); err != nil {
		return Schema{}, err
	}
	applied := map[int64]bool{}
	if exists {
		// goose deletes a version's row when it migrates down; older goose
		// versions append a row with is_applied = false instead.
		rows, err := db.Query(ctx, `
			SELECT DISTINCT ON (version_id) version_id, is_applied FROM goose_db_version
			WHERE version_id > 0 ORDER BY version_id, id DESC`)
		if err != nil {
			return Schema{}, err
		}
		type row struct {
			version int64
			applied bool
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			err := r.Scan(&x.version, &x.applied)
			return x, err
		})
		if err != nil {
			return Schema{}, err
		}
		for _, x := range list {
			applied[x.version] = x.applied
		}
	}
	s := Schema{Latest: versions[len(versions)-1]}
	for v, ok := range applied {
		if ok && v > s.Version {
			s.Version = v
		}
	}
	for _, v := range versions {
		if !applied[v] {
			s.Pending = append(s.Pending, v)
		}
	}
	return s, nil
}

// sourceVersions lists the versions of the embedded migrations, ascending.
func sourceVersions() ([]int64, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var vs []int64
	for _, e := range entries {
		v, err := goose.NumericComponent(e.Name())
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		vs = append(vs, v)
	}
	if len(vs) == 0 {
		return nil, errors.New("no migrations embedded")
	}
	slices.Sort(vs)
	return vs, nil
}
