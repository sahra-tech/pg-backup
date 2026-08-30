package config

import (
	"strings"
	"testing"
)

// minimal is the smallest environment that loads successfully.
func minimal() map[string]string {
	return map[string]string{
		"PGHOST":                 "localhost",
		"PGUSER":                 "postgres",
		"PG_BACKUP_STORAGE_TYPE": "local",
		"PG_BACKUP_LOCAL_PATH":   "./backups",
		"PG_BACKUP_SCHEDULE":     "0 2 * * *",
	}
}

// lookupFrom isolates tests from the real environment.
func lookupFrom(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func loadWith(t *testing.T, vars map[string]string) (*Config, error) {
	t.Helper()
	return load(lookupFrom(vars))
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := loadWith(t, minimal())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Database.Port != 5432 {
		t.Errorf("Port = %d, want 5432", cfg.Database.Port)
	}
	if cfg.Database.SSLMode != "prefer" {
		t.Errorf("SSLMode = %q, want \"prefer\"", cfg.Database.SSLMode)
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, want 30", cfg.RetentionDays)
	}
	if cfg.HealthCheckPort != 8080 {
		t.Errorf("HealthCheckPort = %d, want 8080", cfg.HealthCheckPort)
	}
	if cfg.Storage.S3.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", cfg.Storage.S3.Region)
	}
	// No log file configured means stderr-only, which is what containers want.
	if cfg.LogFile != "" {
		t.Errorf("LogFile = %q, want empty (stderr-only) by default", cfg.LogFile)
	}
	if cfg.RunOnStart || cfg.FullDump {
		t.Error("RunOnStart and FullDump should default to false")
	}
	if cfg.Compression() != DefaultCompressionLevel {
		t.Errorf("Compression() = %d, want %d", cfg.Compression(), DefaultCompressionLevel)
	}
}

// 0 means "store uncompressed" and must survive, unlike an unset value.
func TestCompressionLevelZeroIsHonoured(t *testing.T) {
	vars := minimal()
	vars["PG_BACKUP_COMPRESSION_LEVEL"] = "0"
	cfg, err := loadWith(t, vars)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Compression() != 0 {
		t.Errorf("Compression() = %d, want 0", cfg.Compression())
	}
}

func TestDatabaseListParsing(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,c ", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
		{"solo", []string{"solo"}},
		{"", nil},
		{"  ", nil},
	}
	for _, tt := range tests {
		vars := minimal()
		vars["PG_BACKUP_DATABASES"] = tt.in
		cfg, err := loadWith(t, vars)
		if err != nil {
			t.Fatalf("load(%q): %v", tt.in, err)
		}
		if len(cfg.Database.Databases) != len(tt.want) {
			t.Errorf("%q -> %v, want %v", tt.in, cfg.Database.Databases, tt.want)
			continue
		}
		for i := range tt.want {
			if cfg.Database.Databases[i] != tt.want[i] {
				t.Errorf("%q -> %v, want %v", tt.in, cfg.Database.Databases, tt.want)
				break
			}
		}
	}
}

func TestBooleanParsing(t *testing.T) {
	for _, truthy := range []string{"true", "TRUE", "True", "1", "t"} {
		vars := minimal()
		vars["PG_BACKUP_FULL_DUMP"] = truthy
		cfg, err := loadWith(t, vars)
		if err != nil {
			t.Fatalf("load(%q): %v", truthy, err)
		}
		if !cfg.FullDump {
			t.Errorf("%q should parse as true", truthy)
		}
	}

	vars := minimal()
	vars["PG_BACKUP_FULL_DUMP"] = "yes-please"
	if _, err := loadWith(t, vars); err == nil {
		t.Error("expected an error for an unparseable boolean")
	}
}

// A misconfigured deployment should learn about every problem in one run.
func TestAllErrorsReportedTogether(t *testing.T) {
	_, err := loadWith(t, map[string]string{
		"PG_BACKUP_STORAGE_TYPE": "local",
		"PG_BACKUP_LOCAL_PATH":   "./backups",
		"PG_BACKUP_SCHEDULE":     "0 2 * * *",
	})
	if err == nil {
		t.Fatal("expected errors for the missing required variables")
	}
	for _, want := range []string{"PGHOST", "PGUSER"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s, got: %v", want, err)
		}
	}
}

// Parse failures and validation failures must surface together, or an operator
// fixes one variable per restart.
func TestParseAndValidationErrorsReportedTogether(t *testing.T) {
	vars := minimal()
	vars["PG_BACKUP_FULL_DUMP"] = "maybe"     // parse error
	vars["PGPORT"] = "99999"                  // validation error
	vars["PG_BACKUP_SCHEDULE"] = "not a cron" // validation error

	_, err := loadWith(t, vars)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"PG_BACKUP_FULL_DUMP", "PGPORT", "PG_BACKUP_SCHEDULE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s, got:\n%v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "3 problems") {
		t.Errorf("error should count the problems, got:\n%v", err)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"missing host", func(v map[string]string) { delete(v, "PGHOST") }, "PGHOST is required"},
		{"missing user", func(v map[string]string) { delete(v, "PGUSER") }, "PGUSER is required"},
		{"missing schedule", func(v map[string]string) { delete(v, "PG_BACKUP_SCHEDULE") }, "PG_BACKUP_SCHEDULE is required"},
		{"bad cron", func(v map[string]string) { v["PG_BACKUP_SCHEDULE"] = "not a cron" }, "not a valid cron"},
		{"bad sslmode", func(v map[string]string) { v["PGSSLMODE"] = "bogus" }, "PGSSLMODE"},
		{"bad storage type", func(v map[string]string) { v["PG_BACKUP_STORAGE_TYPE"] = "ftp" }, "is invalid"},
		{"local without path", func(v map[string]string) { delete(v, "PG_BACKUP_LOCAL_PATH") }, "PG_BACKUP_LOCAL_PATH is required"},
		{"negative retention", func(v map[string]string) { v["PG_BACKUP_RETENTION_DAYS"] = "-1" }, "must not be negative"},
		{"compression out of range", func(v map[string]string) { v["PG_BACKUP_COMPRESSION_LEVEL"] = "12" }, "out of range"},
		{"port not a number", func(v map[string]string) { v["PGPORT"] = "abc" }, "whole number"},
		{"port out of range", func(v map[string]string) { v["PGPORT"] = "70000" }, "out of range"},
		{"health port out of range", func(v map[string]string) { v["PG_BACKUP_HEALTH_PORT"] = "0" }, "out of range"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := minimal()
			tt.mutate(vars)
			_, err := loadWith(t, vars)
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestS3RequiresCredentials(t *testing.T) {
	vars := minimal()
	vars["PG_BACKUP_STORAGE_TYPE"] = "s3"
	_, err := loadWith(t, vars)
	if err == nil {
		t.Fatal("expected an error for missing S3 settings")
	}
	for _, want := range []string{"PG_BACKUP_S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got: %v", want, err)
		}
	}
}

// The endpoint is optional: AWS S3 uses the regional resolver.
func TestS3EndpointOptional(t *testing.T) {
	vars := minimal()
	vars["PG_BACKUP_STORAGE_TYPE"] = "s3"
	vars["PG_BACKUP_S3_BUCKET"] = "b"
	vars["AWS_ACCESS_KEY_ID"] = "k"
	vars["AWS_SECRET_ACCESS_KEY"] = "s"
	vars["AWS_REGION"] = "ir-thr-at1"

	cfg, err := loadWith(t, vars)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Storage.S3.Endpoint != "" {
		t.Errorf("Endpoint = %q, want empty", cfg.Storage.S3.Endpoint)
	}
	if cfg.Storage.S3.Region != "ir-thr-at1" {
		t.Errorf("Region = %q", cfg.Storage.S3.Region)
	}
}

func TestValidCronExpressionsAccepted(t *testing.T) {
	for _, schedule := range []string{"0 2 * * *", "@daily", "@every 1h30m", "*/15 * * * *"} {
		vars := minimal()
		vars["PG_BACKUP_SCHEDULE"] = schedule
		if _, err := loadWith(t, vars); err != nil {
			t.Errorf("schedule %q should be valid: %v", schedule, err)
		}
	}
}

// Load reads the real process environment.
func TestLoadReadsProcessEnvironment(t *testing.T) {
	for k, v := range minimal() {
		t.Setenv(k, v)
	}
	t.Setenv("PGPASSWORD", "hunter2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Password != "hunter2" {
		t.Errorf("Password = %q", cfg.Database.Password)
	}
}
