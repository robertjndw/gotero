package pg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

var _ repo.Store = (*Store)(nil)

// Store implements repo.Store on top of a pgx pool.
type Store struct {
	pool *pgxpool.Pool
	cfg  config
}

type config struct {
	lockTimeout      time.Duration
	statementTimeout time.Duration
	acquireTimeout   time.Duration
}

// Option tunes a Store.
type Option func(*config)

// WithLockTimeout bounds how long a library write waits for the row lock
// before failing with repo.ErrBusy. Zero disables the timeout.
func WithLockTimeout(d time.Duration) Option { return func(c *config) { c.lockTimeout = d } }

// WithStatementTimeout bounds every statement inside a library write
// transaction, so a runaway query cannot hold the library lock forever.
func WithStatementTimeout(d time.Duration) Option {
	return func(c *config) { c.statementTimeout = d }
}

// WithAcquireTimeout bounds how long a library read or write waits for a pool
// connection before failing with repo.ErrBusy. Without it a saturated pool
// queues requests until their clients give up. Zero disables the timeout.
func WithAcquireTimeout(d time.Duration) Option { return func(c *config) { c.acquireTimeout = d } }

func New(pool *pgxpool.Pool, opts ...Option) *Store {
	cfg := config{lockTimeout: 10 * time.Second, statementTimeout: 2 * time.Minute, acquireTimeout: 10 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	return &Store{pool: pool, cfg: cfg}
}

func (s *Store) Users() repo.UserRepo                 { return userRepo{s.pool} }
func (s *Store) Keys() repo.KeyRepo                   { return keyRepo{s.pool} }
func (s *Store) Groups() repo.GroupRepo               { return groupRepo{s.pool} }
func (s *Store) LoginSessions() repo.LoginSessionRepo { return loginSessionRepo{s.pool} }
func (s *Store) WebSessions() repo.WebSessionRepo     { return webSessionRepo{s.pool} }
func (s *Store) Invites() repo.InviteRepo             { return inviteRepo{s.pool} }
func (s *Store) Settings() repo.InstanceSettingsRepo  { return instanceSettingsRepo{s.pool} }
func (s *Store) Files() repo.FileRepo                 { return fileRepo{s.pool} }
func (s *Store) Imports() repo.ImportRepo             { return importRepo{s.pool} }

// acquire takes a pool connection, waiting at most the acquire timeout. Only
// that wait maps to ErrBusy; the caller's own cancellation or deadline is
// passed on as it is, so a client that hung up is not reported as overload.
func (s *Store) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	actx := ctx
	if s.cfg.acquireTimeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, s.cfg.acquireTimeout)
		defer cancel()
	}
	conn, err := s.pool.Acquire(actx)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("acquire connection: %w: %w", repo.ErrBusy, err)
		}
		return nil, wrap("acquire connection", err)
	}
	return conn, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// querier is the subset of pgxpool.Pool and pgx.Tx the repositories need, so
// the same read code serves lock-free reads and reads inside a write tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

const (
	pgUniqueViolation = "23505"
	pgFKViolation     = "23503"
	pgLockNotAvail    = "55P03"
)

// transientCodes are SQLSTATEs where retrying later can succeed: the server
// is overloaded, restarting or gave up on us, not the request is wrong.
var transientCodes = map[string]bool{
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
	"57014": true, // query_canceled, e.g. statement_timeout
	"53300": true, // too_many_connections
	"57P01": true, // admin_shutdown
	"57P02": true, // crash_shutdown
	"57P03": true, // cannot_connect_now
}

// mapErr translates driver errors into the repo sentinels and leaves every
// other error alone. It is idempotent, so wrapped errors can pass through
// several layers.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrConflict) || errors.Is(err, repo.ErrBusy) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %w", repo.ErrNotFound, err)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case pgUniqueViolation:
			return fmt.Errorf("%w: %w", repo.ErrConflict, err)
		case pgFKViolation:
			// An insert pointing at a row that does not exist.
			return fmt.Errorf("%w: %w", repo.ErrNotFound, err)
		case pgLockNotAvail:
			return fmt.Errorf("%w: %w", repo.ErrBusy, err)
		}
		// Class 08 is every connection exception.
		if transientCodes[pe.Code] || strings.HasPrefix(pe.Code, "08") {
			return fmt.Errorf("%w: %w", repo.ErrBusy, err)
		}
	}
	if pgconn.Timeout(err) {
		return fmt.Errorf("%w: %w", repo.ErrBusy, err)
	}
	return err
}

// mapDeleteErr is mapErr for DELETEs, where a foreign key violation means
// something still references the row rather than that something is missing.
func mapDeleteErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == pgFKViolation {
		return fmt.Errorf("%w: %w", repo.ErrConflict, err)
	}
	return mapErr(err)
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, mapErr(err))
}

// requireRow turns "0 rows affected" into ErrNotFound.
func requireRow(tag pgconn.CommandTag, err error, op string) error {
	if err != nil {
		return wrap(op, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", op, repo.ErrNotFound)
	}
	return nil
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) + "ms" }

// Prune removes expired short-lived rows and old delete-log entries (unless
// keepDeletedFor <= 0). It returns the total number of rows removed.
func (s *Store) Prune(ctx context.Context, now time.Time, keepDeletedFor time.Duration) (int64, error) {
	var total int64
	stmts := []string{
		`DELETE FROM login_sessions WHERE expires_at < $1`,
		`DELETE FROM web_sessions WHERE expires_at < $1`,
		// Registered tokens are kept until expiry only so a replayed register
		// is recognized; after that nothing reads them.
		`DELETE FROM upload_tokens WHERE expires_at < $1`,
		`DELETE FROM write_tokens WHERE created_at < $1::timestamptz - interval '12 hours'`,
		`DELETE FROM invites WHERE used_by IS NULL AND used_at IS NULL AND expires_at < $1::timestamptz - interval '30 days'`,
	}
	for _, q := range stmts {
		tag, err := s.pool.Exec(ctx, q, now)
		if err != nil {
			return total, wrap("prune", err)
		}
		total += tag.RowsAffected()
	}

	// Zero or less means the log is kept forever.
	if keepDeletedFor <= 0 {
		return total, nil
	}
	n, err := s.pruneDeletedLog(ctx, now.Add(-keepDeletedFor))
	return total + n, err
}

// pruneDeletedLog works one library at a time under the same row lock writers
// take. Locking the library first keeps the lock order identical to
// InLibrary (library row, then deleted_log rows), which rules out deadlocks.
// Busy libraries are skipped and picked up by the next run.
func (s *Store) pruneDeletedLog(ctx context.Context, cutoff time.Time) (int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT library_id FROM deleted_log WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, wrap("prune deleted log", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, wrap("prune deleted log", err)
	}

	var total int64
	for _, id := range ids {
		n, err := s.pruneLibraryLog(ctx, id, cutoff)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *Store) pruneLibraryLog(ctx context.Context, id int64, cutoff time.Time) (n int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, wrap("prune deleted log", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var locked int64
	err = tx.QueryRow(ctx, `SELECT id FROM libraries WHERE id = $1 FOR UPDATE SKIP LOCKED`, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, wrap("prune deleted log", err)
	}

	var maxVersion *int64
	err = tx.QueryRow(ctx, `
		WITH pruned AS (
			DELETE FROM deleted_log WHERE library_id = $1 AND created_at < $2 RETURNING version
		)
		SELECT count(*), max(version) FROM pruned`, id, cutoff).Scan(&n, &maxVersion)
	if err != nil {
		return 0, wrap("prune deleted log", err)
	}
	if maxVersion != nil {
		if _, err = tx.Exec(ctx,
			`UPDATE libraries SET delete_log_floor = GREATEST(delete_log_floor, $2) WHERE id = $1`,
			id, *maxVersion); err != nil {
			return 0, wrap("raise delete log floor", err)
		}
	}
	return n, wrap("prune deleted log", tx.Commit(ctx))
}

// Stats uses plain counts. Items is the biggest table, but these run on a
// manual admin page, not on a hot path.
func (s *Store) Stats(ctx context.Context) (repo.Stats, error) {
	var st repo.Stats
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM users),
		       (SELECT count(*) FROM groups),
		       (SELECT count(*) FROM items),
		       (SELECT count(*) FROM attachment_files)`).
		Scan(&st.Users, &st.Groups, &st.Items, &st.Attachments)
	return st, wrap("stats", err)
}
