// Package store keeps the service's durable record in Postgres: the legs nex registered and one row per console session with its
// timing and counters. Ids only (PIDs, gathering ids): no names, no keys (owner rule: everything permanent in Postgres, no
// identity copies). The world itself lives in the Java server's data volume.
package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `CREATE TABLE IF NOT EXISTS legs (
	id         bigserial PRIMARY KEY,
	gathering  bigint NOT NULL,
	host_pid   bigint NOT NULL,
	joiner_pid bigint NOT NULL DEFAULT 0,
	created    timestamptz NOT NULL DEFAULT now(),
	connected  timestamptz,
	peer       text NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS sessions (
	id         bigserial PRIMARY KEY,
	leg_id     bigint NOT NULL,
	joiner_pid bigint NOT NULL DEFAULT 0,
	started    timestamptz NOT NULL DEFAULT now(),
	ended      timestamptz,
	end_reason text NOT NULL DEFAULT '',
	chunks     bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sessions_open ON sessions (started) WHERE ended IS NULL;`

type Store struct{ DB *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	// sessions left open by a previous process ended with it
	db.Exec(ctx, `UPDATE sessions SET ended=now(), end_reason='restart' WHERE ended IS NULL`)
	return &Store{DB: db}, nil
}

func (s *Store) Close() { s.DB.Close() }

func (s *Store) CreateLeg(ctx context.Context, gathering, hostPID, joinerPID int64) (id int64, err error) {
	err = s.DB.QueryRow(ctx, `INSERT INTO legs (gathering, host_pid, joiner_pid) VALUES ($1,$2,$3) RETURNING id`, gathering, hostPID, joinerPID).Scan(&id)
	return
}

func (s *Store) Connected(ctx context.Context, leg int64, peer string) error {
	_, err := s.DB.Exec(ctx, `UPDATE legs SET connected=now(), peer=$2 WHERE id=$1`, leg, peer)
	return err
}

func (s *Store) StartSession(ctx context.Context, leg, joinerPID int64) (id int64, err error) {
	err = s.DB.QueryRow(ctx, `INSERT INTO sessions (leg_id, joiner_pid) VALUES ($1,$2) RETURNING id`, leg, joinerPID).Scan(&id)
	return
}

func (s *Store) EndSession(ctx context.Context, id int64, reason string, chunks int64) error {
	_, err := s.DB.Exec(ctx, `UPDATE sessions SET ended=now(), end_reason=$2, chunks=$3 WHERE id=$1 AND ended IS NULL`, id, reason, chunks)
	return err
}

// Online counts open sessions.
func (s *Store) Online(ctx context.Context) (n int, err error) {
	err = s.DB.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE ended IS NULL`).Scan(&n)
	return
}
