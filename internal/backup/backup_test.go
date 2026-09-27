package backup

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"pg-backup/internal/config"
	"pg-backup/internal/logger"
	"pg-backup/internal/storage"
)

// fakeStorage records what was committed, so tests can assert that a failed
// dump never reaches storage.
type fakeStorage struct {
	mu       sync.Mutex
	stored   map[string][]byte
	objects  []storage.Object
	deleted  []string
	storeErr error
	listErr  error
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{stored: map[string][]byte{}}
}

func (f *fakeStorage) Store(_ context.Context, filename string, data io.Reader) error {
	body, err := io.ReadAll(data)
	if err != nil {
		// Mirror the real providers: a failed stream commits nothing.
		return err
	}
	if f.storeErr != nil {
		return f.storeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored[filename] = body
	return nil
}

func (f *fakeStorage) List(_ context.Context) ([]storage.Object, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.objects, nil
}

func (f *fakeStorage) Delete(_ context.Context, filename string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, filename)
	return nil
}

func testService(t *testing.T, cfg *config.Config, store storage.Provider) *Service {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	return NewService(cfg, store, logger.NewTo(io.Discard))
}

func TestQuoteDSNValue(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"simple", "'simple'"},
		{"with space", "'with space'"},
		{`has'quote`, `'has\'quote'`},
		{`back\slash`, `'back\\slash'`},
		{"", "''"},
	}
	for _, tt := range tests {
		if got := quoteDSNValue(tt.in); got != tt.want {
			t.Errorf("quoteDSNValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A password with a space would previously truncate the DSN and let the
// remainder be parsed as further connection parameters.
func TestConnStringQuotesAwkwardPassword(t *testing.T) {
	cfg := &config.Config{}
	cfg.Database.Host = "db.internal"
	cfg.Database.Port = 5432
	cfg.Database.User = "postgres"
	cfg.Database.SSLMode = "require"
	cfg.Database.Password = "pa ss' sslmode=disable"

	got := testService(t, cfg, newFakeStorage()).connString("mydb")

	if !strings.Contains(got, `password='pa ss\' sslmode=disable'`) {
		t.Errorf("password not quoted in DSN: %s", got)
	}
	if !strings.Contains(got, "sslmode='require'") {
		t.Errorf("configured sslmode missing: %s", got)
	}
	// The injected sslmode must remain inside the quoted password.
	if strings.Count(got, "sslmode=") != 2 || !strings.HasSuffix(got, `'`) {
		t.Errorf("unexpected DSN shape: %s", got)
	}
}

func TestConnStringOmitsEmptyPassword(t *testing.T) {
	cfg := &config.Config{}
	cfg.Database.Host = "h"
	cfg.Database.User = "u"
	cfg.Database.SSLMode = "prefer"
	if got := testService(t, cfg, newFakeStorage()).connString("db"); strings.Contains(got, "password") {
		t.Errorf("empty password should be omitted, got: %s", got)
	}
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	return string(out)
}

func TestRunDumpStoresCompressedOutput(t *testing.T) {
	store := newFakeStorage()
	svc := testService(t, nil, store)

	cmd := exec.Command("sh", "-c", "printf 'CREATE TABLE t();'")
	if err := svc.runDump(context.Background(), cmd, "db_2024-01-01_00-00-00.sql.gz", "fake_dump"); err != nil {
		t.Fatalf("runDump: %v", err)
	}

	body, ok := store.stored["db_2024-01-01_00-00-00.sql.gz"]
	if !ok {
		t.Fatal("nothing was stored")
	}
	if got := gunzip(t, body); got != "CREATE TABLE t();" {
		t.Errorf("stored content = %q", got)
	}
}

// The critical streaming invariant: if the dump command exits non-zero, the
// bytes it already produced must not be committed as a backup.
func TestRunDumpDiscardsOutputWhenCommandFails(t *testing.T) {
	store := newFakeStorage()
	svc := testService(t, nil, store)

	cmd := exec.Command("sh", "-c", "printf 'half a dump'; echo 'boom' >&2; exit 3")
	err := svc.runDump(context.Background(), cmd, "db_2024-01-01_00-00-00.sql.gz", "fake_dump")
	if err == nil {
		t.Fatal("expected an error when the dump command fails")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry stderr, got: %v", err)
	}
	if len(store.stored) != 0 {
		t.Errorf("partial dump was committed: %v", store.stored)
	}
}

// A storage failure must not deadlock the writer goroutine.
func TestRunDumpHandlesStorageFailure(t *testing.T) {
	store := newFakeStorage()
	store.storeErr = errors.New("bucket unreachable")
	svc := testService(t, nil, store)

	done := make(chan error, 1)
	go func() {
		cmd := exec.Command("sh", "-c", "printf 'x%.0s' $(seq 1 200000)")
		done <- svc.runDump(context.Background(), cmd, "db_2024-01-01_00-00-00.sql.gz", "fake_dump")
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "bucket unreachable") {
			t.Errorf("err = %v, want the storage error", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runDump deadlocked when storage failed")
	}
}

// earlyFailStorage gives up after reading only part of the stream, the way the
// S3 uploader does when CreateMultipartUpload fails after buffering the first
// part.
type earlyFailStorage struct {
	fakeStorage
	err error
}

func (f *earlyFailStorage) Store(_ context.Context, _ string, data io.Reader) error {
	_, _ = io.CopyN(io.Discard, data, 64*1024)
	return f.err
}

// Regression: when storage failed without draining the pipe, the dump command
// blocked forever on a full stdout, runDump never returned, and the held
// single-run slot made every later scheduled backup skip.
func TestRunDumpDoesNotHangWhenStorageFailsEarly(t *testing.T) {
	store := &earlyFailStorage{err: errors.New("bucket unreachable")}
	svc := testService(t, nil, store)

	done := make(chan error, 1)
	go func() {
		// Produces output forever: only killing it can end the dump.
		cmd := exec.Command("yes")
		done <- svc.runDump(context.Background(), cmd, "db_2024-01-01_00-00-00.sql.gz", "fake_dump")
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "bucket unreachable") {
			t.Fatalf("err = %v, want the storage error", err)
		}
		if !strings.Contains(err.Error(), "failed to store backup") {
			t.Errorf("storage failure misreported as a dump failure: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runDump hung after storage failed early")
	}
}

func TestRunDumpRespectsContextCancellation(t *testing.T) {
	svc := testService(t, nil, newFakeStorage())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30; echo done")
	if err := svc.runDump(ctx, cmd, "db_2024-01-01_00-00-00.sql.gz", "fake_dump"); err == nil {
		t.Error("expected an error for a cancelled context")
	}
}

func TestRatioGuardsAgainstEmptyDump(t *testing.T) {
	if got := ratio(0, 0); got != "n/a" {
		t.Errorf("ratio(0,0) = %q, want \"n/a\"", got)
	}
	if got := ratio(1000, 250); got != "25.0%" {
		t.Errorf("ratio(1000,250) = %q, want \"25.0%%\"", got)
	}
}

func TestTimestampedNameMatchesBackupPattern(t *testing.T) {
	for _, prefix := range []string{"mydb", "full_dump", "db-with-dashes"} {
		name := timestampedName(prefix)
		if !strings.HasPrefix(name, prefix+"_") {
			t.Errorf("%q does not start with %q", name, prefix)
		}
		if !backupFilePattern.MatchString(name) {
			t.Errorf("generated name %q is not recognised by the retention pattern", name)
		}
	}
}

// Two names generated in the same second must differ, or a manual trigger
// could overwrite a scheduled backup.
func TestTimestampedNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		name := timestampedName("db")
		if seen[name] {
			t.Fatalf("duplicate backup name generated: %s", name)
		}
		seen[name] = true
	}
}

func TestBackupFilePattern(t *testing.T) {
	matches := []string{
		"mydb_2024-01-02_03-04-05.sql.gz", // written by older versions
		"mydb_2024-01-02_03-04-05_a1b2c3.sql.gz",
		"full_dump_2024-01-02_03-04-05_ffffff.sql.gz",
	}
	for _, name := range matches {
		if !backupFilePattern.MatchString(name) {
			t.Errorf("%q should match", name)
		}
	}

	nonMatches := []string{
		"important-customer-data.sql.gz",
		"notes.txt",
		"mydb_2024-01-02.sql.gz",
		"terraform.tfstate",
		"mydb_2024-01-02_03-04-05.sql",
	}
	for _, name := range nonMatches {
		if backupFilePattern.MatchString(name) {
			t.Errorf("%q must NOT match: retention would delete unrelated data", name)
		}
	}
}

func TestPruneDeletesOnlyExpiredBackups(t *testing.T) {
	store := newFakeStorage()
	now := time.Now()
	store.objects = []storage.Object{
		{Name: "old_2024-01-02_03-04-05_a1b2c3.sql.gz", ModTime: now.Add(-40 * 24 * time.Hour)},
		{Name: "fresh_2024-01-02_03-04-05_a1b2c3.sql.gz", ModTime: now.Add(-1 * time.Hour)},
		{Name: "boundary_2024-01-02_03-04-05_a1b2c3.sql.gz", ModTime: now.Add(-29 * 24 * time.Hour)},
		// Not ours: must survive regardless of age.
		{Name: "someone-elses-archive.tar.gz", ModTime: now.Add(-365 * 24 * time.Hour)},
		{Name: "notes.txt", ModTime: now.Add(-365 * 24 * time.Hour)},
	}

	cfg := &config.Config{RetentionDays: 30}
	if err := testService(t, cfg, store).pruneOldBackups(context.Background()); err != nil {
		t.Fatalf("pruneOldBackups: %v", err)
	}

	if len(store.deleted) != 1 || store.deleted[0] != "old_2024-01-02_03-04-05_a1b2c3.sql.gz" {
		t.Errorf("deleted = %v, want only the expired backup", store.deleted)
	}
}

func TestPruneDisabledWhenRetentionZero(t *testing.T) {
	store := newFakeStorage()
	store.objects = []storage.Object{
		{Name: "ancient_2020-01-02_03-04-05_a1b2c3.sql.gz", ModTime: time.Now().Add(-10000 * time.Hour)},
	}
	cfg := &config.Config{RetentionDays: 0}
	if err := testService(t, cfg, store).pruneOldBackups(context.Background()); err != nil {
		t.Fatalf("pruneOldBackups: %v", err)
	}
	if len(store.deleted) != 0 {
		t.Errorf("retention 0 should disable pruning, deleted %v", store.deleted)
	}
}

// A failed cycle must not trigger a sweep: we may be about to delete the only
// copies of data we just failed to re-dump.
func TestBackupAllSkipsPruneAfterFailure(t *testing.T) {
	store := newFakeStorage()
	store.objects = []storage.Object{
		{Name: "old_2024-01-02_03-04-05_a1b2c3.sql.gz", ModTime: time.Now().Add(-100 * 24 * time.Hour)},
	}

	cfg := &config.Config{RetentionDays: 30, FullDump: true}
	cfg.Database.Host = "127.0.0.1"
	cfg.Database.Port = 1 // nothing is listening
	cfg.Database.User = "nobody"

	svc := testService(t, cfg, store)
	if _, err := svc.BackupAll(context.Background()); err == nil {
		t.Fatal("expected the backup to fail")
	}
	if len(store.deleted) != 0 {
		t.Errorf("prune ran after a failed cycle, deleted: %v", store.deleted)
	}
}

func TestDumpEnvExtendsProcessEnvironment(t *testing.T) {
	t.Setenv("PG_BACKUP_CANARY", "present")

	cfg := &config.Config{}
	cfg.Database.Password = "hunter2"
	env := testService(t, cfg, newFakeStorage()).dumpEnv()

	var sawCanary, sawPath, sawPassword bool
	for _, kv := range env {
		switch {
		case kv == "PG_BACKUP_CANARY=present":
			sawCanary = true
		case strings.HasPrefix(kv, "PATH="):
			sawPath = true
		case kv == "PGPASSWORD=hunter2":
			sawPassword = true
		}
	}
	if !sawCanary || !sawPath {
		t.Error("dumpEnv dropped the process environment; pg_dumpall would not find pg_dump")
	}
	if !sawPassword {
		t.Error("PGPASSWORD was not passed to the dump command")
	}
}

func TestDumpEnvOmitsEmptyPassword(t *testing.T) {
	env := testService(t, &config.Config{}, newFakeStorage()).dumpEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "PGPASSWORD=") {
			t.Errorf("PGPASSWORD should not be set when no password is configured")
		}
	}
}
