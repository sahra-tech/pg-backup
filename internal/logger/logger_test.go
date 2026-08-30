package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr redirects os.Stderr for the duration of fn.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn()
	w.Close()
	return <-done
}

// The reason this package tees at all: `docker logs` reads stdout/stderr, so a
// file-only logger leaves containerised deployments with no visible output.
func TestLogsAlwaysReachStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.log")

	out := captureStderr(t, func() {
		log, err := New(path)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		log.Info("backup started for %s", "analytics")
		log.Warning("a warning")
		log.Error("an error")
		log.Close()
	})

	for _, want := range []string{"INFO: backup started for analytics", "WARNING: a warning", "ERROR: an error"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, out)
		}
	}

	// The file must receive the same lines.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(body), "backup started for analytics") {
		t.Errorf("log file missing the entry; got:\n%s", body)
	}
}

// No log file configured is a valid, container-friendly setup.
func TestEmptyFilenameLogsToStderrOnly(t *testing.T) {
	out := captureStderr(t, func() {
		log, err := New("")
		if err != nil {
			t.Fatalf("New(\"\") should not error: %v", err)
		}
		log.Info("still visible")
		if err := log.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if !strings.Contains(out, "still visible") {
		t.Errorf("stderr missing the entry; got:\n%s", out)
	}
}

// An unwritable log path must degrade to stderr, never stop the process: the
// image runs as non-root and a bind-mounted log directory may be root-owned.
func TestUnwritableLogFileDegradesToStderr(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	var log *Logger
	var newErr error
	out := captureStderr(t, func() {
		log, newErr = New(filepath.Join(dir, "backup.log"))
		if log == nil {
			t.Fatal("New must return a usable logger even when the file fails")
		}
		log.Info("backups keep running")
		log.Close()
	})

	if newErr == nil {
		t.Error("expected an error describing the unwritable log file")
	}
	if !strings.Contains(out, "backups keep running") {
		t.Errorf("logging must continue on stderr; got:\n%s", out)
	}
}
