package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLegacyUpgradePreservesRecordsAndFutureVersionIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	original, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = original.Exec(`CREATE TABLE records (bucket TEXT NOT NULL,id TEXT NOT NULL,value BLOB NOT NULL,PRIMARY KEY(bucket,id)); INSERT INTO records VALUES ('fixture','keep','{"value":42}');`)
	if err != nil {
		t.Fatal(err)
	}
	original.Close()
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var record struct{ Value int }
	if err := upgraded.Get(t.Context(), "fixture", "keep", &record); err != nil || record.Value != 42 {
		t.Fatal(record, err)
	}
	var version int
	_ = upgraded.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != SchemaVersion {
		t.Fatal(version)
	}
	if _, err := upgraded.db.Exec(`PRAGMA user_version=99`); err != nil {
		t.Fatal(err)
	}
	upgraded.Close()
	if next, err := Open(path); err == nil {
		next.Close()
		t.Fatal("future database opened")
	}
	future, _ := sql.Open(driverName, path)
	defer future.Close()
	_ = future.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != 99 {
		t.Fatal("future version modified")
	}
	var value string
	if err := future.QueryRow(`SELECT value FROM records WHERE bucket='fixture' AND id='keep'`).Scan(&value); err != nil || value != `{"value":42}` {
		t.Fatal("future data changed", value, err)
	}
}
