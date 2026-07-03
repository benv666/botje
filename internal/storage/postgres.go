package storage

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Postgres is the production Store: the kv table from
// migrations/0001_kv.sql, writes committed per Put.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects, applies pending embedded migrations, and
// returns the store. Safe to call from every boot: applied migrations
// are recorded in schema_migrations and skipped.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: connect: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	if err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	// ReadDir returns entries sorted by name, so 0001, 0002, ... in order
	for _, e := range entries {
		if err := applyMigration(ctx, pool, e.Name()); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, name string) error {
	sql, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// take the version row as a lock so concurrent boots don't race
		tag, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, name)
		if err != nil {
			return fmt.Errorf("storage: record migration %s: %w", name, err)
		}
		if tag.RowsAffected() == 0 {
			return nil // already applied
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("storage: apply migration %s: %w", name, err)
		}
		return nil
	})
}

func (p *Postgres) Get(ns, name string, dst any) (bool, error) {
	var raw []byte
	err := p.pool.QueryRow(context.Background(),
		`SELECT value FROM kv WHERE namespace = $1 AND name = $2`, ns, name).Scan(&raw)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, dst)
}

func (p *Postgres) Put(ns, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(context.Background(),
		`INSERT INTO kv (namespace, name, value) VALUES ($1, $2, $3)
		 ON CONFLICT (namespace, name) DO UPDATE SET value = $3, updated_at = now()`,
		ns, name, raw)
	return err
}

func (p *Postgres) Delete(ns, name string) error {
	_, err := p.pool.Exec(context.Background(),
		`DELETE FROM kv WHERE namespace = $1 AND name = $2`, ns, name)
	return err
}

func (p *Postgres) Names(ns string) ([]string, error) {
	rows, err := p.pool.Query(context.Background(),
		`SELECT name FROM kv WHERE namespace = $1 ORDER BY name`, ns)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

// wipe empties the kv table; test helper.
func (p *Postgres) wipe(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `TRUNCATE kv`)
	return err
}
