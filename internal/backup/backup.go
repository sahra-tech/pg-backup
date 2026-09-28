package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"pg-backup/internal/config"
	"pg-backup/internal/logger"
	"pg-backup/internal/storage"

	_ "github.com/lib/pq"
)

// timestampLayout is kept stable so that files written by older versions are
// still recognised by backupFilePattern and remain eligible for pruning.
const timestampLayout = "2006-01-02_15-04-05"

// backupFilePattern matches files this tool created. The retention sweep only
// ever deletes names matching it, so pointing the tool at a shared bucket or
// directory cannot destroy unrelated data. The trailing suffix group is
// optional to keep matching files written before suffixes were added.
var backupFilePattern = regexp.MustCompile(`_\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}(_[0-9a-f]{6})?\.sql\.gz$`)

type Service struct {
	cfg     *config.Config
	storage storage.Provider
	logger  *logger.Logger
}

func NewService(cfg *config.Config, store storage.Provider, log *logger.Logger) *Service {
	return &Service{
		cfg:     cfg,
		storage: store,
		logger:  log,
	}
}

// BackupAll runs one backup cycle and prunes expired backups. It returns the
// number of targets successfully backed up.
func (s *Service) BackupAll(ctx context.Context) (int, error) {
	succeeded, failed, err := s.runCycle(ctx)

	switch {
	case err == nil:
		s.runPrune(ctx, nil)
	case len(failed) == 0 || ctx.Err() != nil:
		// We cannot tell which backups this cycle refreshed (discovery or the
		// full dump failed, or we were cancelled), so delete nothing.
		s.logger.Warning("Skipping retention sweep because this cycle reported failures")
	default:
		// Never prune a database we just failed to re-dump: its old copies may
		// be the only good ones. Everything else was refreshed and is still
		// pruned, so one persistently failing database cannot stop retention
		// for the whole bucket.
		s.logger.Warning("Retention sweep keeps all backups of failed databases: %s",
			strings.Join(failed, ", "))
		protect := make(map[string]bool, len(failed))
		for _, name := range failed {
			protect[name] = true
		}
		s.runPrune(ctx, protect)
	}
	return succeeded, err
}

func (s *Service) runPrune(ctx context.Context, protect map[string]bool) {
	if pruneErr := s.pruneOldBackups(ctx, protect); pruneErr != nil {
		// The backup itself succeeded; a failed sweep must not mask that.
		s.logger.Error("Retention sweep failed: %v", pruneErr)
	}
}

// runCycle backs up every target. On error, failed names the databases whose
// backup did not complete; it is empty when the scope of the failure is
// unknown.
func (s *Service) runCycle(ctx context.Context) (int, []string, error) {
	if s.cfg.FullDump {
		s.logger.Info("Full dump mode enabled, creating single backup file for entire server")
		if err := s.backupFullServer(ctx); err != nil {
			s.logger.Error("Failed to perform full dump: %v", err)
			return 0, nil, err
		}
		s.logger.Info("Full dump completed successfully")
		return 1, nil, nil
	}

	databases := s.cfg.Database.Databases

	if len(databases) == 0 {
		s.logger.Info("No specific databases configured, discovering all databases")
		discovered, err := s.discoverDatabases(ctx)
		if err != nil {
			s.logger.Error("Failed to discover databases: %v", err)
			return 0, nil, err
		}
		databases = discovered
		s.logger.Info("Discovered %d databases: %s", len(databases), strings.Join(databases, ", "))
	}

	// Back up everything we can, then report the failures together. Aborting on
	// the first error would silently skip every remaining database.
	var (
		succeeded int
		failed    []string
		failures  []error
	)
	for _, database := range databases {
		if err := ctx.Err(); err != nil {
			failures = append(failures, fmt.Errorf("aborted before %q: %w", database, err))
			break
		}

		s.logger.Info("Starting backup for database: %s", database)
		if err := s.backupDatabase(ctx, database); err != nil {
			s.logger.Error("Failed to backup database %s: %v", database, err)
			failed = append(failed, database)
			failures = append(failures, fmt.Errorf("database %q: %w", database, err))
			continue
		}
		succeeded++
		s.logger.Info("Successfully backed up database: %s", database)
	}

	if len(failures) > 0 {
		return succeeded, failed, fmt.Errorf("%d of %d databases failed: %w",
			len(failures), len(databases), errors.Join(failures...))
	}
	return succeeded, nil, nil
}

// quoteDSNValue renders a libpq connection-string value safely. Without this a
// password containing a space or quote would truncate the DSN or inject
// adjacent parameters.
func quoteDSNValue(v string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + replacer.Replace(v) + "'"
}

func (s *Service) connString(dbname string) string {
	parts := []string{
		"host=" + quoteDSNValue(s.cfg.Database.Host),
		fmt.Sprintf("port=%d", s.cfg.Database.Port),
		"user=" + quoteDSNValue(s.cfg.Database.User),
		"dbname=" + quoteDSNValue(dbname),
		"sslmode=" + quoteDSNValue(s.cfg.Database.SSLMode),
	}
	if s.cfg.Database.Password != "" {
		parts = append(parts, "password="+quoteDSNValue(s.cfg.Database.Password))
	}
	return strings.Join(parts, " ")
}

func (s *Service) discoverDatabases(ctx context.Context) ([]string, error) {
	db, err := sql.Open("postgres", s.connString("postgres"))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping PostgreSQL: %w", err)
	}

	const query = `
		SELECT datname
		FROM pg_database
		WHERE datistemplate = false
		AND datallowconn = true
		AND datname NOT IN ('postgres')
		ORDER BY datname
	`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query databases: %w", err)
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			return nil, fmt.Errorf("failed to scan database name: %w", err)
		}
		databases = append(databases, dbName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating database rows: %w", err)
	}

	return databases, nil
}

// dumpEnv extends the process environment rather than replacing it. Assigning
// to a nil cmd.Env would hand the child a single-variable environment with no
// PATH, TZ or locale -- which is why pg_dumpall could not find pg_dump.
func (s *Service) dumpEnv() []string {
	env := os.Environ()
	if s.cfg.Database.Password != "" {
		env = append(env, "PGPASSWORD="+s.cfg.Database.Password)
	}
	return env
}

func timestampedName(prefix string) string {
	var suffix [3]byte
	// A short random suffix keeps a manual trigger from overwriting a scheduled
	// backup that started in the same second.
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Sprintf("%s_%s.sql.gz", prefix, time.Now().UTC().Format(timestampLayout))
	}
	return fmt.Sprintf("%s_%s_%s.sql.gz",
		prefix, time.Now().UTC().Format(timestampLayout), hex.EncodeToString(suffix[:]))
}

func (s *Service) backupDatabase(ctx context.Context, database string) error {
	cmd := exec.CommandContext(ctx, "pg_dump",
		"-h", s.cfg.Database.Host,
		"-p", fmt.Sprintf("%d", s.cfg.Database.Port),
		"-U", s.cfg.Database.User,
		"-d", database,
		"--no-password",
	)
	cmd.Env = s.dumpEnv()

	return s.runDump(ctx, cmd, timestampedName(database), "pg_dump")
}

func (s *Service) backupFullServer(ctx context.Context) error {
	if _, err := exec.LookPath("pg_dumpall"); err != nil {
		s.logger.Error("pg_dumpall not found on PATH. Full dump requires the PostgreSQL client tools.")
		s.logger.Error("Set full_dump: false to back up each database individually with pg_dump.")
		return fmt.Errorf("pg_dumpall not available: %w", err)
	}

	cmd := exec.CommandContext(ctx, "pg_dumpall",
		"-h", s.cfg.Database.Host,
		"-p", fmt.Sprintf("%d", s.cfg.Database.Port),
		"-U", s.cfg.Database.User,
		"--no-password",
	)
	cmd.Env = s.dumpEnv()

	return s.runDump(ctx, cmd, timestampedName("full_dump"), "pg_dumpall")
}

// countingWriter records how many bytes passed through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// runDump streams the command's stdout through gzip straight into storage,
// so peak memory stays flat regardless of dump size. If the command exits
// non-zero the pipe is closed with an error, which makes the storage provider
// discard the partial object instead of committing a truncated backup.
func (s *Service) runDump(ctx context.Context, cmd *exec.Cmd, filename, tool string) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to open %s stdout: %w", tool, err)
	}

	// stderr stays buffered: it is small and needed for error messages.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	s.logger.Info("Executing %s -> %s", tool, filename)
	start := time.Now()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", tool, err)
	}

	pr, pw := io.Pipe()
	var (
		rawBytes   int64
		dumpErr    error
		compressed = &countingWriter{w: pw}
		done       = make(chan struct{})
	)

	go func() {
		defer close(done)

		gz, err := gzip.NewWriterLevel(compressed, s.cfg.Compression())
		if err != nil {
			dumpErr = err
			pw.CloseWithError(err)
			// Kill before Wait: nothing reads stdout, so the child would
			// otherwise block on a full pipe and Wait would never return.
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return
		}

		rawBytes, err = io.Copy(gz, stdout)
		copyErr := err
		if copyErr != nil {
			// Usually storage gave up and closed the pipe. Nobody will read
			// stdout again, so the child blocks on a full pipe forever and
			// Wait never returns -- holding the single-run slot and silently
			// skipping every later backup. Kill it; the dump is lost anyway.
			_ = cmd.Process.Kill()
		}
		closeErr := gz.Close()

		// Wait only after stdout is drained (or the child killed), and only
		// decide the stream was clean once the exit status is known.
		waitErr := cmd.Wait()

		switch {
		case copyErr != nil:
			dumpErr = fmt.Errorf("failed reading %s output: %w", tool, copyErr)
		case waitErr != nil:
			dumpErr = fmt.Errorf("%s failed: %w (stderr: %s)",
				tool, waitErr, strings.TrimSpace(stderr.String()))
		case closeErr != nil:
			dumpErr = fmt.Errorf("failed to finish compression: %w", closeErr)
		}
		if dumpErr != nil {
			pw.CloseWithError(dumpErr)
			return
		}
		pw.Close()
	}()

	storeErr := s.storage.Store(ctx, filename, pr)
	// Unblock the writer if Store bailed out before draining the pipe.
	pr.CloseWithError(storeErr)
	<-done

	// A failing dump surfaces at the provider as a read error, so report the
	// dump's own error rather than blaming storage for it. The reverse holds
	// too: when storage fails first, the writer sees storeErr on the pipe, and
	// that is a storage failure, not a problem reading the dump.
	if storeErr != nil && errors.Is(dumpErr, storeErr) {
		return fmt.Errorf("failed to store backup %s: %w", filename, storeErr)
	}
	if dumpErr != nil {
		return dumpErr
	}
	if storeErr != nil {
		return fmt.Errorf("failed to store backup %s: %w", filename, storeErr)
	}

	if stderr.Len() > 0 {
		s.logger.Warning("%s warnings: %s", tool, strings.TrimSpace(stderr.String()))
	}

	s.logger.Info("Stored %s in %v (raw: %d bytes, compressed: %d bytes, ratio: %s)",
		filename, time.Since(start).Round(time.Millisecond),
		rawBytes, compressed.n, ratio(rawBytes, compressed.n))
	return nil
}

// ratio formats the compressed size as a percentage of the original, guarding
// against the divide-by-zero an empty dump would otherwise produce.
func ratio(raw, compressed int64) string {
	if raw == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(compressed)/float64(raw)*100)
}

// pruneOldBackups deletes backups older than the configured retention window.
// It only considers names this tool produces, and never touches backups whose
// database name is in protect.
func (s *Service) pruneOldBackups(ctx context.Context, protect map[string]bool) error {
	if s.cfg.RetentionDays <= 0 {
		return nil
	}

	cutoff := time.Now().Add(-time.Duration(s.cfg.RetentionDays) * 24 * time.Hour)
	objects, err := s.storage.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list stored backups: %w", err)
	}

	var (
		deleted  int
		freed    int64
		expired  int
		failures []error
	)
	for _, obj := range objects {
		loc := backupFilePattern.FindStringIndex(obj.Name)
		if loc == nil {
			continue
		}
		if !obj.ModTime.Before(cutoff) {
			continue
		}
		expired++
		if protect[obj.Name[:loc[0]]] {
			continue
		}
		if err := s.storage.Delete(ctx, obj.Name); err != nil {
			failures = append(failures, fmt.Errorf("delete %q: %w", obj.Name, err))
			continue
		}
		s.logger.Info("Pruned expired backup: %s (age %s)",
			obj.Name, time.Since(obj.ModTime).Round(time.Hour))
		deleted++
		freed += obj.Size
	}

	// Always report, so an operator can confirm from the logs that the sweep
	// ran even when there was nothing to delete.
	s.logger.Info("Retention sweep: %d objects listed, %d older than %d days, %d removed (%d bytes)",
		len(objects), expired, s.cfg.RetentionDays, deleted, freed)
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}
