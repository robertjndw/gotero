package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
)

// Every migration ships a Down section, and an operator rolling back a bad
// release will run them. Nothing else executes them, so a Down that drops
// the wrong thing or leaves debris behind would only surface on the day it
// is needed. This walks the whole chain down and back up, one step at a
// time, and requires the schema at every version to match what the Up path
// produced at that version.
func TestMigrationsRollBackCleanly(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.New(t)
	_, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)

	p, db, err := pg.Provider(pool)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Climb one version at a time so we know what each version looks like
	// when reached the normal way.
	want := map[int64]string{0: schemaSnapshot(t, pool)}
	var versions []int64
	for {
		r, err := p.UpByOne(ctx)
		if err != nil {
			require.ErrorContains(t, err, "no next version")
			break
		}
		v := r.Source.Version
		versions = append(versions, v)
		want[v] = schemaSnapshot(t, pool)
	}
	require.NotEmpty(t, versions)

	for i := len(versions) - 1; i >= 0; i-- {
		_, err := p.Down(ctx)
		require.NoError(t, err, "down from version %d", versions[i])
		prev := int64(0)
		if i > 0 {
			prev = versions[i-1]
		}
		got := schemaSnapshot(t, pool)
		if prev == 0 {
			// The init Down leaves citext installed on purpose: dropping an
			// extension needs privileges managed Postgres often withholds,
			// and Up only creates it IF NOT EXISTS.
			got = strings.Replace(got, "extension citext\n", "", 1)
		}
		require.Equal(t, want[prev], got,
			"schema after rolling back %d differs from schema at %d", versions[i], prev)
	}

	// Rolling back must leave a database that upgrades again, otherwise a
	// rollback strands the operator on the old release.
	_, err = p.Up(ctx)
	require.NoError(t, err)
	require.Equal(t, want[versions[len(versions)-1]], schemaSnapshot(t, pool))
}

// schemaSnapshot renders the public schema as sorted text: tables and their
// columns, constraints, indexes, extensions, functions and triggers. Object
// names are included so a Down that recreates something under a different
// name still counts as a difference.
func schemaSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type
		       || ' null=' || is_nullable || ' default=' || coalesce(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name <> 'goose_db_version'
		UNION ALL
		SELECT 'constraint ' || conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE connamespace = 'public'::regnamespace AND conrelid::regclass::text <> 'goose_db_version'
		UNION ALL
		SELECT 'index ' || indexdef
		FROM pg_indexes
		WHERE schemaname = 'public' AND tablename <> 'goose_db_version'
		UNION ALL
		SELECT 'extension ' || extname FROM pg_extension
		UNION ALL
		SELECT 'sequence ' || sequence_name || ' ' || data_type
		FROM information_schema.sequences
		WHERE sequence_schema = 'public'
		UNION ALL
		SELECT 'function ' || p.oid::regprocedure::text || ' ' || md5(pg_get_functiondef(p.oid))
		FROM pg_proc p
		LEFT JOIN pg_depend d ON d.objid = p.oid AND d.deptype = 'e'
		WHERE p.pronamespace = 'public'::regnamespace AND d.objid IS NULL
		UNION ALL
		SELECT 'trigger ' || tgrelid::regclass || ' ' || tgname || ' ' || pg_get_triggerdef(oid)
		FROM pg_trigger
		WHERE NOT tgisinternal AND tgrelid::regclass::text NOT LIKE 'pg\_%'
		UNION ALL
		SELECT 'type ' || t.typname
		FROM pg_type t
		LEFT JOIN pg_depend d ON d.objid = t.oid AND d.deptype = 'e'
		WHERE t.typnamespace = 'public'::regnamespace AND t.typtype IN ('e', 'd') AND d.objid IS NULL
		ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		b.WriteString(line)
		b.WriteByte('\n')
	}
	require.NoError(t, rows.Err())
	return b.String()
}
