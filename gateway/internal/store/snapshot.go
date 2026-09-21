package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// Inspect opens an existing database read-only without migrations or record output.
func Inspect(ctx context.Context, path string) error {
	db, err := openSnapshotSource(path)
	if err != nil {
		return err
	}
	defer db.Close()
	return inspect(ctx, db)
}

func openSnapshotSource(path string) (*sql.DB, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("existing regular database required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro"}
	db, err := sql.Open(driverName, uri.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func inspect(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > SchemaVersion {
		return errors.New("unsupported database schema")
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("database integrity check failed")
	}
	var records int
	// Check the expected table and JSON values without exporting any private data.
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM records WHERE bucket IS NULL OR id IS NULL OR NOT json_valid(value)").Scan(&records); err != nil {
		return err
	}
	if records != 0 {
		return errors.New("invalid state records")
	}
	return nil
}

// Snapshot produces a consistent private backup, or a fresh restore candidate.
// No existing destination (including a symlink) is replaced, even under a race.
func Snapshot(ctx context.Context, source, destination string) error {
	db, err := openSnapshotSource(source)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = inspect(ctx, db); err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		return errors.New("snapshot destination must not exist")
	}
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".zombie-state-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	temporary := filepath.Join(dir, "snapshot.db")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	file.Close()
	if _, err = db.ExecContext(ctx, "VACUUM INTO ?", temporary); err != nil {
		return err
	}
	if err = Inspect(ctx, temporary); err != nil {
		return err
	}
	file, err = os.OpenFile(temporary, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = file.Sync()
	file.Close()
	if err != nil {
		return err
	}
	if err = os.Link(temporary, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
