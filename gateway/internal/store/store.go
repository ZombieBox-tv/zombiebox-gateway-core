// Package store persists bounded domain records in a private SQLite database.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"zombiebox.local/gateway/internal/domain"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		f.Close()
		if err := os.Chmod(path, 0600); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS records (bucket TEXT NOT NULL, id TEXT NOT NULL, value BLOB NOT NULL, PRIMARY KEY(bucket,id));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

type Record = domain.Record

// PutMany commits related records together, including device/token rotation.
func (s *Store) PutMany(ctx context.Context, records ...Record) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range records {
		data, err := json.Marshal(r.Value)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO records(bucket,id,value) VALUES(?,?,?) ON CONFLICT(bucket,id) DO UPDATE SET value=excluded.value`, r.Bucket, r.ID, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Put(ctx context.Context, bucket, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO records(bucket,id,value) VALUES(?,?,?) ON CONFLICT(bucket,id) DO UPDATE SET value=excluded.value`, bucket, id, data)
	return err
}
func (s *Store) Get(ctx context.Context, bucket, id string, out any) error {
	var data []byte
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM records WHERE bucket=? AND id=?`, bucket, id).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return err
	}
	return json.Unmarshal(data, out)
}
func (s *Store) Count(ctx context.Context, bucket string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE bucket=?`, bucket).Scan(&n)
	return n, err
}
func (s *Store) List(ctx context.Context, bucket string) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT value FROM records WHERE bucket=? ORDER BY id LIMIT 256`, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(b))
	}
	return out, rows.Err()
}
func (s *Store) Delete(ctx context.Context, bucket, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM records WHERE bucket=? AND id=?`, bucket, id)
	return err
}
