package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotRestoresCommittedWALAndNeverReplacesDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	db, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if err = db.Put(ctx, "private", "fixture", map[string]string{"secret": "kept-private"}); err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(dir, "copy.db")
	if err = Snapshot(ctx, source, copy); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(copy)
	if info.Mode().Perm() != 0600 {
		t.Fatal("public snapshot permissions", info.Mode())
	}
	restored, err := Open(copy)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var value map[string]string
	if restored.Get(ctx, "private", "fixture", &value) != nil || value["secret"] != "kept-private" {
		t.Fatal("lost committed WAL state")
	}
	if Snapshot(ctx, source, copy) == nil {
		t.Fatal("replaced existing database")
	}
	link := filepath.Join(dir, "symlink.db")
	os.Symlink(source, link)
	if Snapshot(ctx, source, link) == nil {
		t.Fatal("replaced symlink")
	}
	if Inspect(ctx, link) == nil {
		t.Fatal("followed source symlink")
	}
	if _, err = db.db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	if Inspect(ctx, source) == nil {
		t.Fatal("newer schema accepted")
	}
	if Snapshot(ctx, source, filepath.Join(dir, "newer.db")) == nil {
		t.Fatal("newer schema snapshotted")
	}
}
