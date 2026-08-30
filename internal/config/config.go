// Package config loads pg-backup's settings from the process environment.
//
// Standard PostgreSQL (PG*) and AWS (AWS_*) variables are honoured where they
// exist so the tool composes with existing tooling; everything specific to this
// application is prefixed PG_BACKUP_.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
)

// DefaultCompressionLevel matches compress/gzip's DefaultCompression.
const DefaultCompressionLevel = -1

type Database struct {
	Host      string
	Port      int
	User      string
	Password  string
	SSLMode   string
	Databases []string
}

type LocalStorage struct {
	Path string
}

type S3Storage struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
}

type Storage struct {
	Type  string
	Local LocalStorage
	S3    S3Storage
}

type Config struct {
	Database Database
	Storage  Storage

	Schedule         string
	LogFile          string
	RunOnStart       bool
	RetentionDays    int
	HealthCheckPort  int
	HealthCheckBind  string
	FullDump         bool
	CompressionLevel *int
	TriggerToken     string
}

// Compression returns the gzip level to use, defaulting to gzip.DefaultCompression.
func (c *Config) Compression() int {
	if c.CompressionLevel == nil {
		return DefaultCompressionLevel
	}
	return *c.CompressionLevel
}

// env reads settings from a lookup function, collecting every problem rather
// than stopping at the first: a misconfigured deployment should learn about all
// of its mistakes in one run.
type env struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (e *env) fail(format string, args ...any) {
	e.errs = append(e.errs, fmt.Errorf(format, args...))
}

func (e *env) str(key, def string) string {
	if v, ok := e.lookup(key); ok && v != "" {
		return v
	}
	return def
}

func (e *env) required(key string) string {
	v := e.str(key, "")
	if v == "" {
		e.fail("%s is required", key)
	}
	return v
}

func (e *env) int(key string, def int) int {
	raw, ok := e.lookup(key)
	if !ok || raw == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		e.fail("%s must be a whole number, got %q", key, raw)
		return def
	}
	return n
}

// optInt distinguishes "unset" from "set to 0", which matters for
// PG_BACKUP_COMPRESSION_LEVEL where 0 means "store uncompressed".
func (e *env) optInt(key string) *int {
	raw, ok := e.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		e.fail("%s must be a whole number, got %q", key, raw)
		return nil
	}
	return &n
}

func (e *env) bool(key string, def bool) bool {
	raw, ok := e.lookup(key)
	if !ok || raw == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		e.fail("%s must be true or false, got %q", key, raw)
		return def
	}
	return b
}

// list splits a comma-separated value, ignoring blanks and surrounding spaces.
func (e *env) list(key string) []string {
	raw, ok := e.lookup(key)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (*Config, error) {
	e := &env{lookup: lookup}
	var config Config

	config.Database = Database{
		Host:      e.required("PGHOST"),
		Port:      e.int("PGPORT", 5432),
		User:      e.required("PGUSER"),
		Password:  e.str("PGPASSWORD", ""),
		SSLMode:   e.str("PGSSLMODE", "prefer"),
		Databases: e.list("PG_BACKUP_DATABASES"),
	}

	config.Storage = Storage{
		Type:  e.required("PG_BACKUP_STORAGE_TYPE"),
		Local: LocalStorage{Path: e.str("PG_BACKUP_LOCAL_PATH", "")},
		S3: S3Storage{
			Bucket:    e.str("PG_BACKUP_S3_BUCKET", ""),
			Region:    e.str("AWS_REGION", "us-east-1"),
			Endpoint:  e.str("PG_BACKUP_S3_ENDPOINT", ""),
			AccessKey: e.str("AWS_ACCESS_KEY_ID", ""),
			SecretKey: e.str("AWS_SECRET_ACCESS_KEY", ""),
		},
	}

	config.Schedule = e.required("PG_BACKUP_SCHEDULE")
	// Optional: stderr always receives logs, which is what containers read.
	config.LogFile = e.str("PG_BACKUP_LOG_FILE", "")
	config.RunOnStart = e.bool("PG_BACKUP_RUN_ON_START", false)
	config.RetentionDays = e.int("PG_BACKUP_RETENTION_DAYS", 30)
	config.HealthCheckPort = e.int("PG_BACKUP_HEALTH_PORT", 8080)
	config.HealthCheckBind = e.str("PG_BACKUP_HEALTH_BIND", "")
	config.FullDump = e.bool("PG_BACKUP_FULL_DUMP", false)
	config.CompressionLevel = e.optInt("PG_BACKUP_COMPRESSION_LEVEL")
	config.TriggerToken = e.str("PG_BACKUP_TRIGGER_TOKEN", "")

	// Validate even when parsing produced errors, so every problem surfaces in
	// one run. Parse failures fall back to defaults, so validate sees sane
	// values and will not double-report the same field.
	problems := append(e.errs, validate(&config)...)
	if len(problems) > 0 {
		return nil, configErrors(problems)
	}

	return &config, nil
}

// configErrors renders every problem as its own line, which reads far better
// than a single run-on message when several variables are wrong at once.
type configErrors []error

func (c configErrors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problem", len(c))
	if len(c) != 1 {
		b.WriteString("s")
	}
	b.WriteString("):")
	for _, err := range c {
		b.WriteString("\n  - " + err.Error())
	}
	return b.String()
}

// Unwrap lets errors.Is and errors.As reach the individual problems.
func (c configErrors) Unwrap() []error { return c }

// validSSLModes are the values libpq accepts for sslmode.
var validSSLModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true,
	"require": true, "verify-ca": true, "verify-full": true,
}

func validate(config *Config) []error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if config.Database.Port < 1 || config.Database.Port > 65535 {
		fail("PGPORT %d is out of range", config.Database.Port)
	}
	if !validSSLModes[config.Database.SSLMode] {
		fail("PGSSLMODE %q is invalid", config.Database.SSLMode)
	}

	switch config.Storage.Type {
	case "":
		// already reported as missing
	case "local":
		if config.Storage.Local.Path == "" {
			fail("PG_BACKUP_LOCAL_PATH is required when PG_BACKUP_STORAGE_TYPE=local")
		}
	case "s3":
		var missing []string
		if config.Storage.S3.Bucket == "" {
			missing = append(missing, "PG_BACKUP_S3_BUCKET")
		}
		if config.Storage.S3.AccessKey == "" {
			missing = append(missing, "AWS_ACCESS_KEY_ID")
		}
		if config.Storage.S3.SecretKey == "" {
			missing = append(missing, "AWS_SECRET_ACCESS_KEY")
		}
		if len(missing) > 0 {
			fail("PG_BACKUP_STORAGE_TYPE=s3 requires %s", strings.Join(missing, ", "))
		}
	default:
		fail("PG_BACKUP_STORAGE_TYPE %q is invalid (want \"local\" or \"s3\")", config.Storage.Type)
	}

	if config.Schedule != "" {
		if _, err := cron.ParseStandard(config.Schedule); err != nil {
			fail("PG_BACKUP_SCHEDULE %q is not a valid cron expression: %v", config.Schedule, err)
		}
	}
	if config.RetentionDays < 0 {
		fail("PG_BACKUP_RETENTION_DAYS must not be negative")
	}
	if lvl := config.Compression(); lvl < -2 || lvl > 9 {
		fail("PG_BACKUP_COMPRESSION_LEVEL %d is out of range (-1..9)", lvl)
	}
	if config.HealthCheckPort < 1 || config.HealthCheckPort > 65535 {
		fail("PG_BACKUP_HEALTH_PORT %d is out of range", config.HealthCheckPort)
	}

	return errs
}
