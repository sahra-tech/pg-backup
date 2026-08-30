package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalStoreAndList(t *testing.T) {
	dir := t.TempDir()
	local := NewLocal(filepath.Join(dir, "backups"))

	if err := local.Store(context.Background(), "db_2024-01-01_00-00-00.sql.gz", strings.NewReader("payload")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	objects, err := local.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("List returned %d objects, want 1", len(objects))
	}
	if objects[0].Name != "db_2024-01-01_00-00-00.sql.gz" {
		t.Errorf("Name = %q", objects[0].Name)
	}
	if objects[0].Size != int64(len("payload")) {
		t.Errorf("Size = %d, want %d", objects[0].Size, len("payload"))
	}
}

// A missing backup directory is not an error; there is simply nothing stored.
func TestLocalListMissingDirectory(t *testing.T) {
	local := NewLocal(filepath.Join(t.TempDir(), "does-not-exist"))
	objects, err := local.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("List returned %d objects, want 0", len(objects))
	}
}

// errReader fails partway through, standing in for a dump whose command died.
type errReader struct {
	data []byte
	err  error
	read bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if !e.read {
		e.read = true
		n := copy(p, e.data)
		return n, nil
	}
	return 0, e.err
}

// The central guarantee: a failed stream must leave nothing that looks like a
// finished backup.
func TestLocalStoreDiscardsPartialWrite(t *testing.T) {
	base := filepath.Join(t.TempDir(), "backups")
	local := NewLocal(base)

	wantErr := errors.New("dump exploded")
	err := local.Store(context.Background(), "db_2024-01-01_00-00-00.sql.gz", &errReader{data: []byte("half a dump"), err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Store error = %v, want %v", err, wantErr)
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("failed store left files behind: %v", names)
	}
}

func TestLocalDelete(t *testing.T) {
	base := filepath.Join(t.TempDir(), "backups")
	local := NewLocal(base)
	if err := local.Store(context.Background(), "db_2024-01-01_00-00-00.sql.gz", strings.NewReader("x")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := local.Delete(context.Background(), "db_2024-01-01_00-00-00.sql.gz"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	objects, _ := local.List(context.Background())
	if len(objects) != 0 {
		t.Errorf("object still present after Delete")
	}
	// Deleting something already gone is not an error.
	if err := local.Delete(context.Background(), "db_2024-01-01_00-00-00.sql.gz"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

// Delete must never be talked into touching files outside the backup directory.
func TestLocalDeleteRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "important.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	local := NewLocal(filepath.Join(dir, "backups"))
	if err := local.Delete(context.Background(), "../important.txt"); err == nil {
		t.Error("expected Delete to reject a traversing path")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("file outside the backup directory was removed: %v", err)
	}
}

func TestLocalStoreRoundTrip(t *testing.T) {
	local := NewLocal(t.TempDir())
	const content = "some backup bytes"
	if err := local.Store(context.Background(), "db_2024-01-01_00-00-00.sql.gz", strings.NewReader(content)); err != nil {
		t.Fatalf("Store: %v", err)
	}
	objects, _ := local.List(context.Background())
	f, err := os.Open(filepath.Join(local.basePath, objects[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != content {
		t.Errorf("round trip = %q, want %q", got, content)
	}
	if objects[0].ModTime.After(time.Now().Add(time.Minute)) {
		t.Errorf("implausible ModTime %v", objects[0].ModTime)
	}
}
