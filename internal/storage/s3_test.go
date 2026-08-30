package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// aws-sdk-go v1 accepted a bare host; v2 parses BaseEndpoint as a URL and
// fails without a scheme. Existing configs must keep working across the
// migration.
func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"s3.amazonaws.com", "https://s3.amazonaws.com"},
		{"s3.ir-thr-at1.arvanstorage.ir", "https://s3.ir-thr-at1.arvanstorage.ir"},
		{"https://s3.example.com", "https://s3.example.com"},
		{"http://minio.local:9000", "http://minio.local:9000"},
		{"  s3.example.com  ", "https://s3.example.com"},
	}
	for _, tt := range tests {
		if got := normalizeEndpoint(tt.in); got != tt.want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The reported failure: a CDN/gateway in front of an S3-compatible store
// answers with an HTML error page, which is not parseable as an S3 <Error>.
const gatewayHTML = `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">` +
	`<title></title></head><body><section class="error-section error-502">502</section></body></html>`

func TestStoreReportsGatewayErrorClearly(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(gatewayHTML))
	}))
	defer srv.Close()

	// Two attempts keeps backoff short; the shipped default is asserted below.
	store, err := NewS3("winbox-pg-backup", "ir-thr-at1", srv.URL, "key", "secret",
		WithMaxRetryAttempts(2))
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}

	err = store.Store(context.Background(), "full_dump_2026-08-30_06-04-05_abc123.sql.gz",
		strings.NewReader("a modest dump"))
	if err == nil {
		t.Fatal("expected an error from a 502 gateway")
	}

	// The operator needs the status code and the endpoint, not a hex dump.
	msg := err.Error()
	if !strings.Contains(msg, "502") {
		t.Errorf("error should name the HTTP status, got: %v", err)
	}
	if !strings.Contains(msg, srv.URL) {
		t.Errorf("error should name the endpoint, got: %v", err)
	}
	if !strings.Contains(msg, "gateway/upstream") {
		t.Errorf("error should explain this is not a credentials problem, got: %v", err)
	}

	// A 502 is transient and must be retried, not surfaced on the first try.
	if got := attempts.Load(); got < 2 {
		t.Errorf("made %d attempts, want the request retried", got)
	}
	t.Logf("attempts against the failing gateway: %d", attempts.Load())
}

func TestStoreAndListAgainstFakeS3(t *testing.T) {
	var put, list atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			put.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet:
			list.Add(1)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>b</Name><IsTruncated>false</IsTruncated>
  <Contents><Key>db_2024-01-02_03-04-05_a1b2c3.sql.gz</Key>
    <LastModified>2024-01-02T03:04:05.000Z</LastModified><Size>1234</Size></Contents>
</ListBucketResult>`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	store, err := NewS3("b", "us-east-1", srv.URL, "key", "secret")
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	ctx := context.Background()

	if err := store.Store(ctx, "db_2024-01-02_03-04-05_a1b2c3.sql.gz", strings.NewReader("dump")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if put.Load() == 0 {
		t.Error("no PUT reached the server")
	}

	objects, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("List returned %d objects, want 1", len(objects))
	}
	if objects[0].Name != "db_2024-01-02_03-04-05_a1b2c3.sql.gz" || objects[0].Size != 1234 {
		t.Errorf("unexpected object: %+v", objects[0])
	}
	if objects[0].ModTime.Year() != 2024 {
		t.Errorf("ModTime not parsed: %v", objects[0].ModTime)
	}
}

// A cancelled context must abort promptly rather than burning every retry.
func TestStoreRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	store, err := NewS3("b", "us-east-1", srv.URL, "k", "s", WithMaxRetryAttempts(2))
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Store(ctx, "x_2024-01-02_03-04-05_a1b2c3.sql.gz", strings.NewReader("d")); err == nil {
		t.Error("expected an error for a cancelled context")
	}
}

// Transient gateway errors are common in front of S3-compatible stores, so the
// shipped default must be above the SDK's default of 3.
func TestDefaultRetryAttempts(t *testing.T) {
	if maxRetryAttempts <= 3 {
		t.Errorf("maxRetryAttempts = %d, want more than the SDK default of 3", maxRetryAttempts)
	}
	store, err := NewS3("b", "us-east-1", "", "k", "s")
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if store.maxAttempts != maxRetryAttempts {
		t.Errorf("maxAttempts = %d, want %d", store.maxAttempts, maxRetryAttempts)
	}
}
