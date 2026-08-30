package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pg-backup/internal/logger"
)

// fakeBackup blocks until released, so tests can observe a backup in flight.
type fakeBackup struct {
	calls   atomic.Int32
	release chan struct{}
	err     error
	started chan struct{}
	once    sync.Once
}

func newFakeBackup() *fakeBackup {
	return &fakeBackup{release: make(chan struct{}), started: make(chan struct{})}
}

func (f *fakeBackup) BackupAll(ctx context.Context) (int, error) {
	f.calls.Add(1)
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return 3, f.err
}

func newTestService(t *testing.T, token string) (*Service, *fakeBackup) {
	t.Helper()
	svc := NewService(logger.NewTo(io.Discard), 0, token)
	backup := newFakeBackup()
	svc.SetBackupService(backup)
	return svc, backup
}

func post(t *testing.T, h http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/trigger", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// C5: concurrent triggers must not stack backups.
func TestTriggerRejectsConcurrentBackups(t *testing.T) {
	svc, backup := newTestService(t, "")
	h := svc.Handler()

	if code := post(t, h, "").Code; code != http.StatusAccepted {
		t.Fatalf("first trigger = %d, want 202", code)
	}
	<-backup.started

	for i := 0; i < 10; i++ {
		if code := post(t, h, "").Code; code != http.StatusConflict {
			t.Fatalf("trigger while running = %d, want 409", code)
		}
	}

	close(backup.release)
	if !svc.Wait(testCtx(t)) {
		t.Fatal("Wait timed out")
	}
	if got := backup.calls.Load(); got != 1 {
		t.Errorf("BackupAll called %d times, want 1", got)
	}
}

// The backup must outlive the request that started it. Using the request
// context would cancel it the instant the handler returned.
func TestTriggeredBackupSurvivesRequestCompletion(t *testing.T) {
	svc, backup := newTestService(t, "")
	svc.SetBaseContext(context.Background())

	if code := post(t, svc.Handler(), "").Code; code != http.StatusAccepted {
		t.Fatal("trigger was not accepted")
	}
	<-backup.started

	// The handler has long returned; the backup should still be running.
	time.Sleep(50 * time.Millisecond)
	close(backup.release)
	if !svc.Wait(testCtx(t)) {
		t.Fatal("Wait timed out")
	}

	if svc.backupCount != 1 {
		t.Errorf("backupCount = %d, want 1; the backup was cancelled with the request", svc.backupCount)
	}
}

func TestTriggerRequiresToken(t *testing.T) {
	svc, backup := newTestService(t, "s3cret")
	h := svc.Handler()

	if code := post(t, h, "").Code; code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", code)
	}
	if code := post(t, h, "wrong").Code; code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", code)
	}
	if got := backup.calls.Load(); got != 0 {
		t.Fatalf("unauthorized requests started %d backups", got)
	}

	if code := post(t, h, "s3cret").Code; code != http.StatusAccepted {
		t.Errorf("correct token = %d, want 202", code)
	}
	<-backup.started
	close(backup.release)
	svc.Wait(testCtx(t))
}

func TestTriggerRejectsGet(t *testing.T) {
	svc, _ := newTestService(t, "")
	req := httptest.NewRequest(http.MethodGet, "/trigger", nil)
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /trigger = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow header = %q, want POST", got)
	}
}

// H2: /status must report the scheduler's real next fire time.
func TestStatusReportsConfiguredNextRun(t *testing.T) {
	svc, _ := newTestService(t, "")
	next := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	svc.SetNextRun(func() time.Time { return next })

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := next.Format(time.RFC3339); got.NextBackup != want {
		t.Errorf("NextBackup = %q, want %q", got.NextBackup, want)
	}
	if got.LastBackup != "never" {
		t.Errorf("LastBackup = %q, want \"never\"", got.LastBackup)
	}
}

// H3: the counter must advance once per successful backup, from either path.
func TestBackupCountIncrements(t *testing.T) {
	svc, _ := newTestService(t, "")
	for i := 1; i <= 3; i++ {
		svc.RecordSuccess(time.Now(), 5)
		if svc.backupCount != i {
			t.Fatalf("after %d successes backupCount = %d", i, svc.backupCount)
		}
	}
	if svc.databaseCount != 5 {
		t.Errorf("databaseCount = %d, want 5", svc.databaseCount)
	}
}

// H1: /status is served while backups mutate the same fields. Under -race this
// fails on the unsynchronised version.
func TestStatusIsRaceFreeUnderConcurrentUpdates(t *testing.T) {
	svc, _ := newTestService(t, "")
	svc.SetNextRun(func() time.Time { return time.Now().Add(time.Hour) })
	h := svc.Handler()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					svc.RecordSuccess(time.Now(), 2)
				}
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestHealthEndpoint(t *testing.T) {
	svc, _ := newTestService(t, "")
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("/health = %d, want 200", rec.Code)
	}
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "healthy" {
		t.Errorf("body = %v", body)
	}
}

func TestWaitTimesOutWhileBackupRuns(t *testing.T) {
	svc, backup := newTestService(t, "")
	svc.Run(context.Background())
	<-backup.started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if svc.Wait(ctx) {
		t.Error("Wait returned true while a backup was still running")
	}

	close(backup.release)
	svc.Wait(testCtx(t))
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
