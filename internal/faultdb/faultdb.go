// Package faultdb kills Postgres connections in the middle of a transaction,
// for tests.
//
// It is a pgx tracer. When a statement whose SQL contains a chosen fragment
// is about to be sent, the tracer terminates that connection's backend with
// pg_terminate_backend from a separate connection and waits until it is
// gone. The statement then goes out on a dead connection, which is exactly
// what a database restart, a failover or a network cut looks like to the
// server: everything the transaction did so far is rolled back by Postgres,
// not by gotero.
//
//	k := faultdb.New(dsn)
//	cfg.ConnConfig.Tracer = k
//	k.KillOn("INSERT INTO blobs", 1)
//
// The package is meant for tests only.
package faultdb

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Killer is a pgx.QueryTracer and pgx.BatchTracer. Install it on the pool
// under test; it opens its own connection to do the killing.
type Killer struct {
	dsn string

	mu     sync.Mutex
	rules  []*rule
	killed int
	err    error
}

type rule struct {
	fragment string
	after    int
	times    int
	seen     int
	fired    int
}

var (
	_ pgx.QueryTracer = (*Killer)(nil)
	_ pgx.BatchTracer = (*Killer)(nil)
)

// New returns a Killer that connects to dsn when it has to kill. The role
// must be allowed to terminate the traced connections (a superuser, or the
// same role).
func New(dsn string) *Killer { return &Killer{dsn: dsn} }

// KillOn terminates the connection that is about to run the nth statement
// (1-based) containing fragment, and only that one. Matching is case
// sensitive and counts every statement, batched ones included.
func (k *Killer) KillOn(fragment string, nth int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rules = append(k.rules, &rule{fragment: fragment, after: nth - 1, times: 1})
}

// KillEvery terminates every connection that is about to run a statement
// containing fragment, until Clear.
func (k *Killer) KillEvery(fragment string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rules = append(k.rules, &rule{fragment: fragment})
}

// Clear drops all rules.
func (k *Killer) Clear() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rules = nil
}

// Killed reports how many connections were terminated so far, and the first
// error the killer itself ran into. A test that expected a kill should check
// both: a rule that never matched passes silently otherwise.
func (k *Killer) Killed() (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.killed, k.err
}

func (k *Killer) match(sql string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, r := range k.rules {
		if !strings.Contains(sql, r.fragment) {
			continue
		}
		r.seen++
		if r.seen <= r.after || (r.times > 0 && r.fired >= r.times) {
			continue
		}
		r.fired++
		return true
	}
	return false
}

func (k *Killer) kill(pid uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := func() error {
		c, err := pgx.Connect(ctx, k.dsn)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(context.Background()) }()
		// The timeout form waits until the backend has actually exited, so
		// the traced statement cannot sneak through before the kill lands.
		var ok bool
		if err := c.QueryRow(ctx, `SELECT pg_terminate_backend($1, 10000)`, int32(pid)).Scan(&ok); err != nil { //nolint:gosec // pids fit in int32
			return err
		}
		if !ok {
			return fmt.Errorf("faultdb: backend %d did not terminate", pid)
		}
		return nil
	}()
	k.mu.Lock()
	defer k.mu.Unlock()
	if err != nil {
		if k.err == nil {
			k.err = err
		}
		return
	}
	k.killed++
}

// TraceQueryStart implements pgx.QueryTracer.
func (k *Killer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if k.match(data.SQL) {
		k.kill(conn.PgConn().PID())
	}
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (k *Killer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart implements pgx.BatchTracer.
func (k *Killer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if data.Batch == nil {
		return ctx
	}
	for _, q := range data.Batch.QueuedQueries {
		if k.match(q.SQL) {
			k.kill(conn.PgConn().PID())
			break
		}
	}
	return ctx
}

// TraceBatchQuery implements pgx.BatchTracer.
func (k *Killer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

// TraceBatchEnd implements pgx.BatchTracer.
func (k *Killer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
