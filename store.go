package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

// LockError is returned when a lock is held; Info is the holder's lock JSON.
type LockError struct{ Info []byte }

func (e *LockError) Error() string { return "state is locked" }

// VersionInfo describes one stored state version.
type VersionInfo struct {
	Version   int       `json:"version"`
	Size      int       `json:"size"`
	Deleted   bool      `json:"deleted"` // tombstone written by DELETE
	CreatedAt time.Time `json:"created_at"`
}

// Credential grants a user access to one project, or to all projects when Project is "*".
type Credential struct {
	Username     string    `json:"username"`
	Project      string    `json:"project"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// Store is the persistence interface. Implement it to add non-SQL backends.
type Store interface {
	// GetState returns the given version, or the latest if version <= 0,
	// along with the version number actually returned.
	GetState(ctx context.Context, project, env string, version int) ([]byte, int, error)
	// PutState stores a new version and returns its number. If a lock exists,
	// lockID must match it. Writing bytes identical to the latest version is a
	// no-op that returns the existing version number.
	PutState(ctx context.Context, project, env string, data []byte, lockID string) (int, error)
	// DeleteState writes a tombstone: the current state disappears, history stays.
	DeleteState(ctx context.Context, project, env string) error
	ListVersions(ctx context.Context, project, env string) ([]VersionInfo, error)
	Lock(ctx context.Context, project, env, id string, info []byte) error
	Unlock(ctx context.Context, project, env, id string) error

	GetCredential(ctx context.Context, username string) (Credential, error)
	PutCredential(ctx context.Context, c Credential) error // upsert by username
	DeleteCredential(ctx context.Context, username string) error
	ListCredentials(ctx context.Context) ([]Credential, error)

	Close() error
}

// SQLStore works with any database/sql driver. Supported dialects: sqlite, postgres.
type SQLStore struct {
	db       *sql.DB
	postgres bool
	keep     int // versions to retain per state; 0 = keep all
}

func OpenSQLStore(driver, dsn string, keep int) (*SQLStore, error) {
	var blob string
	switch driver {
	case "sqlite":
		blob = "BLOB"
	case "postgres":
		blob = "BYTEA"
	default:
		return nil, fmt.Errorf("unsupported db driver %q (use sqlite or postgres)", driver)
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	s := &SQLStore{db: db, postgres: driver == "postgres", keep: keep}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1) // sqlite: single writer, avoids SQLITE_BUSY
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS state_versions (
			project    TEXT    NOT NULL,
			env        TEXT    NOT NULL,
			version    INTEGER NOT NULL,
			data       ` + blob + `,
			deleted    BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (project, env, version))`,
		`CREATE TABLE IF NOT EXISTS locks (
			project    TEXT NOT NULL,
			env        TEXT NOT NULL,
			lock_id    TEXT NOT NULL,
			info       TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (project, env))`,
		`CREATE TABLE IF NOT EXISTS credentials (
			username      TEXT NOT NULL PRIMARY KEY,
			project       TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("init schema: %w", err)
		}
	}
	return s, nil
}

func (s *SQLStore) Close() error { return s.db.Close() }

// q rewrites ? placeholders to $n for postgres.
func (s *SQLStore) q(query string) string {
	if !s.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, c := range query {
		if c == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func (s *SQLStore) GetState(ctx context.Context, project, env string, version int) ([]byte, int, error) {
	query := `SELECT version, data, deleted FROM state_versions
		WHERE project=? AND env=? ORDER BY version DESC LIMIT 1`
	args := []any{project, env}
	if version > 0 {
		query = `SELECT version, data, deleted FROM state_versions
			WHERE project=? AND env=? AND version=?`
		args = append(args, version)
	}
	var (
		v       int
		data    []byte
		deleted bool
	)
	err := s.db.QueryRowContext(ctx, s.q(query), args...).Scan(&v, &data, &deleted)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && deleted) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return data, v, nil
}

func (s *SQLStore) PutState(ctx context.Context, project, env string, data []byte, lockID string) (int, error) {
	return s.write(ctx, project, env, data, false, lockID, true)
}

func (s *SQLStore) DeleteState(ctx context.Context, project, env string) error {
	_, err := s.write(ctx, project, env, nil, true, "", false)
	return err
}

// write appends a version (or tombstone). Concurrent unlocked writers can
// collide on the (project, env, version) primary key, so retry a few times.
func (s *SQLStore) write(ctx context.Context, project, env string, data []byte, tombstone bool, lockID string, checkLock bool) (int, error) {
	var (
		v   int
		err error
	)
	for i := 0; i < 3; i++ {
		v, err = s.writeOnce(ctx, project, env, data, tombstone, lockID, checkLock)
		var le *LockError
		if err == nil || errors.As(err, &le) || ctx.Err() != nil {
			return v, err
		}
	}
	return 0, err
}

func (s *SQLStore) writeOnce(ctx context.Context, project, env string, data []byte, tombstone bool, lockID string, checkLock bool) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if checkLock {
		var cur, info string
		err = tx.QueryRowContext(ctx,
			s.q(`SELECT lock_id, info FROM locks WHERE project=? AND env=?`), project, env).Scan(&cur, &info)
		switch {
		case err == nil:
			if lockID != cur {
				return 0, &LockError{Info: []byte(info)}
			}
		case errors.Is(err, sql.ErrNoRows):
		default:
			return 0, err
		}
	}

	var (
		latest        int
		latestData    []byte
		latestDeleted bool
	)
	err = tx.QueryRowContext(ctx, s.q(`SELECT version, data, deleted FROM state_versions
		WHERE project=? AND env=? ORDER BY version DESC LIMIT 1`), project, env).
		Scan(&latest, &latestData, &latestDeleted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	has := err == nil

	if tombstone {
		if !has || latestDeleted {
			return latest, nil // nothing to delete
		}
		data = []byte{}
	} else if has && !latestDeleted && bytes.Equal(latestData, data) {
		return latest, nil // identical to current; don't store a duplicate
	}

	next := latest + 1
	if _, err := tx.ExecContext(ctx, s.q(`
		INSERT INTO state_versions (project, env, version, data, deleted) VALUES (?, ?, ?, ?, ?)`),
		project, env, next, data, tombstone); err != nil {
		return 0, err
	}
	if s.keep > 0 && next > s.keep {
		if _, err := tx.ExecContext(ctx, s.q(
			`DELETE FROM state_versions WHERE project=? AND env=? AND version <= ?`),
			project, env, next-s.keep); err != nil {
			return 0, err
		}
	}
	return next, tx.Commit()
}

func (s *SQLStore) ListVersions(ctx context.Context, project, env string) ([]VersionInfo, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT version, COALESCE(length(data), 0), deleted, created_at
		FROM state_versions WHERE project=? AND env=? ORDER BY version DESC`), project, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionInfo{}
	for rows.Next() {
		var vi VersionInfo
		if err := rows.Scan(&vi.Version, &vi.Size, &vi.Deleted, &vi.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, vi)
	}
	return out, rows.Err()
}

func (s *SQLStore) Lock(ctx context.Context, project, env, id string, info []byte) error {
	for i := 0; i < 3; i++ {
		res, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO locks (project, env, lock_id, info) VALUES (?, ?, ?, ?)
			ON CONFLICT (project, env) DO NOTHING`), project, env, id, string(info))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		var existing string
		err = s.db.QueryRowContext(ctx,
			s.q(`SELECT info FROM locks WHERE project=? AND env=?`), project, env).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			continue // released between insert and select; retry
		}
		if err != nil {
			return err
		}
		return &LockError{Info: []byte(existing)}
	}
	return errors.New("lock contention, try again")
}

func (s *SQLStore) Unlock(ctx context.Context, project, env, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cur, info string
	err = tx.QueryRowContext(ctx,
		s.q(`SELECT lock_id, info FROM locks WHERE project=? AND env=?`), project, env).Scan(&cur, &info)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already unlocked
	}
	if err != nil {
		return err
	}
	if id != cur {
		return &LockError{Info: []byte(info)}
	}
	if _, err := tx.ExecContext(ctx,
		s.q(`DELETE FROM locks WHERE project=? AND env=?`), project, env); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- credentials ----

func (s *SQLStore) GetCredential(ctx context.Context, username string) (Credential, error) {
	c := Credential{Username: username}
	err := s.db.QueryRowContext(ctx,
		s.q(`SELECT project, password_hash FROM credentials WHERE username=?`), username).
		Scan(&c.Project, &c.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) PutCredential(ctx context.Context, c Credential) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO credentials (username, project, password_hash) VALUES (?, ?, ?)
		ON CONFLICT (username) DO UPDATE
		SET project = excluded.project, password_hash = excluded.password_hash`),
		c.Username, c.Project, c.PasswordHash)
	return err
}

func (s *SQLStore) DeleteCredential(ctx context.Context, username string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM credentials WHERE username=?`), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) ListCredentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT username, project, created_at FROM credentials ORDER BY project, username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.Username, &c.Project, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
