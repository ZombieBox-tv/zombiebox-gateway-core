package store

import (
	"database/sql"
	"errors"
)

// SchemaVersion is a durable SQLite format version, separate from protocol/app versions.
const SchemaVersion = 1

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return errors.New("database requires a newer gateway; downgrade refused")
	}
	if version == SchemaVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Version zero includes existing pre-migration gateways. Keep their records
	// unchanged; SQLite makes the table/version transition atomic.
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS records (bucket TEXT NOT NULL, id TEXT NOT NULL, value BLOB NOT NULL, PRIMARY KEY(bucket,id)); PRAGMA user_version=1;`); err != nil {
		return err
	}
	return tx.Commit()
}
