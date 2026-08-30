package health

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pg-backup/internal/logger"
)

type BackupService interface {
	BackupAll(ctx context.Context) (int, error)
}

type Status struct {
	Status        string `json:"status"`
	LastBackup    string `json:"last_backup"`
	NextBackup    string `json:"next_backup"`
	Uptime        string `json:"uptime"`
	BackupCount   int    `json:"backup_count"`
	DatabaseCount int    `json:"database_count"`
	Running       bool   `json:"backup_running"`
}

type Service struct {
	logger *logger.Logger

	// mu guards everything below it. These fields are written from the cron
	// goroutine and from trigger handlers while /status reads them.
	mu            sync.RWMutex
	startTime     time.Time
	lastBackup    time.Time
	backupCount   int
	databaseCount int

	// nextRun reports the scheduler's real next fire time. It is set once at
	// startup and read under mu.
	nextRun func() time.Time

	// running admits one backup at a time, so an unauthenticated flood of
	// /trigger requests cannot stack concurrent pg_dump runs.
	running atomic.Bool

	// inFlight tracks started backups so shutdown can wait for them. Cron jobs
	// return as soon as Run hands off, so cron's own Stop cannot do this.
	inFlight sync.WaitGroup

	backupService BackupService
	token         string
	server        *http.Server

	// baseCtx bounds backups started from HTTP handlers. It must NOT be a
	// request context: those are cancelled as soon as the handler returns,
	// which would kill the backup we just accepted.
	baseCtx context.Context
}

func NewService(log *logger.Logger, databaseCount int, token string) *Service {
	return &Service{
		logger:        log,
		startTime:     time.Now(),
		databaseCount: databaseCount,
		token:         token,
		baseCtx:       context.Background(),
	}
}

func (s *Service) SetBackupService(backupService BackupService) {
	s.backupService = backupService
}

// SetBaseContext installs the lifetime context for backups started via HTTP,
// so a shutdown signal cancels an in-flight manual trigger.
func (s *Service) SetBaseContext(ctx context.Context) {
	s.baseCtx = ctx
}

// SetNextRun installs a callback returning the scheduler's next fire time.
func (s *Service) SetNextRun(f func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRun = f
}

// RecordSuccess marks a completed backup. The counter is owned here so the
// scheduled and manual paths cannot disagree about how it increments.
func (s *Service) RecordSuccess(at time.Time, databaseCount int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastBackup = at
	s.backupCount++
	s.databaseCount = databaseCount
}

// Run executes a backup unless one is already in flight. It reports false if
// another run holds the slot.
func (s *Service) Run(ctx context.Context) (started bool) {
	if !s.running.CompareAndSwap(false, true) {
		return false
	}
	s.inFlight.Add(1)
	go func() {
		defer s.inFlight.Done()
		defer s.running.Store(false)
		count, err := s.backupService.BackupAll(ctx)
		if err != nil {
			s.logger.Error("Backup failed: %v", err)
			return
		}
		s.logger.Info("Backup completed successfully for %d databases", count)
		s.RecordSuccess(time.Now(), count)
	}()
	return true
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.healthHandler)
	mux.HandleFunc("/status", s.statusHandler)
	mux.HandleFunc("/trigger", s.triggerHandler)
	return mux
}

// Start blocks serving the health endpoints. It returns a non-nil error on any
// exit other than a clean Shutdown, so the caller can fail loudly instead of
// leaving a live process with a dead health port.
func (s *Service) Start(bind string, port int) error {
	s.server = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", bind, port),
		Handler:      s.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	if s.token == "" {
		s.logger.Warning("No trigger_token set: POST /trigger is unauthenticated. " +
			"Set trigger_token, or bind the health server to 127.0.0.1.")
	}

	s.logger.Info("Health check server starting on %s", s.server.Addr)
	err := s.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Wait blocks until every started backup has finished, or ctx expires. It
// reports false on timeout.
func (s *Service) Wait(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		s.inFlight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Service) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Service) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}

func (s *Service) statusHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	status := Status{
		Status:        "running",
		Uptime:        time.Since(s.startTime).Round(time.Second).String(),
		BackupCount:   s.backupCount,
		DatabaseCount: s.databaseCount,
		LastBackup:    "never",
		NextBackup:    "not scheduled",
		Running:       s.running.Load(),
	}
	if !s.lastBackup.IsZero() {
		status.LastBackup = s.lastBackup.Format(time.RFC3339)
	}
	if s.nextRun != nil {
		if next := s.nextRun(); !next.IsZero() {
			status.NextBackup = next.Format(time.RFC3339)
		}
	}
	s.mu.RUnlock()

	writeJSON(w, http.StatusOK, status)
}

// authorized reports whether the request carries the configured token. When no
// token is configured the endpoint is open, which Start warns about at startup.
func (s *Service) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if presented == "" {
		presented = r.Header.Get("X-Trigger-Token")
	}
	// Constant-time compare so the token cannot be recovered by timing.
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

func (s *Service) triggerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "Only POST method is allowed",
		})
		return
	}

	if !s.authorized(r) {
		s.logger.Warning("Rejected unauthorized backup trigger from %s", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "Invalid or missing trigger token",
		})
		return
	}

	if s.backupService == nil {
		s.logger.Error("Backup service not available for manual trigger")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "Backup service not available",
			"message": "The backup service is not initialized",
		})
		return
	}

	if !s.Run(s.baseCtx) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "Backup already running",
			"message": "A backup is already in progress; try again once it finishes",
		})
		return
	}

	s.logger.Info("Manual backup triggered via HTTP endpoint")
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":     "accepted",
		"message":    "Backup started successfully",
		"started_at": time.Now().Format(time.RFC3339),
	})
}
