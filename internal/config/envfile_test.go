package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvLine(t *testing.T) {
	tests := []struct {
		line, key, value string
	}{
		{"KEY=value", "KEY", "value"},
		{"  KEY = value  ", "KEY", "value"},
		{"export KEY=value", "KEY", "value"},
		{"KEY=", "KEY", ""},
		{`KEY="quoted value"`, "KEY", "quoted value"},
		{"KEY='literal value'", "KEY", "literal value"},
		{`KEY="line\nbreak"`, "KEY", "line\nbreak"},
		{"KEY=value # trailing comment", "KEY", "value"},
		// A '#' that is part of the value must survive.
		{"PGPASSWORD=pa#ssword", "PGPASSWORD", "pa#ssword"},
		{`PGPASSWORD="pa ss#word"`, "PGPASSWORD", "pa ss#word"},
		// Cron expressions contain spaces and asterisks.
		{"PG_BACKUP_SCHEDULE=0 2 * * *", "PG_BACKUP_SCHEDULE", "0 2 * * *"},
		// Non-assignments.
		{"", "", ""},
		{"   ", "", ""},
		{"# a comment", "", ""},
	}

	for _, tt := range tests {
		key, value, err := parseEnvLine(tt.line)
		if err != nil {
			t.Errorf("parseEnvLine(%q) errored: %v", tt.line, err)
			continue
		}
		if key != tt.key || value != tt.value {
			t.Errorf("parseEnvLine(%q) = (%q, %q), want (%q, %q)", tt.line, key, value, tt.key, tt.value)
		}
	}
}

func TestParseEnvLineRejectsMalformed(t *testing.T) {
	for _, line := range []string{"NOT_AN_ASSIGNMENT", "=novalue"} {
		if _, _, err := parseEnvLine(line); err == nil {
			t.Errorf("parseEnvLine(%q) should have failed", line)
		}
	}
}

func writeEnvFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvFile(t *testing.T) {
	path := writeEnvFile(t, "# comment\n\nPG_BACKUP_TEST_A=one\nexport PG_BACKUP_TEST_B='two'\n")

	t.Setenv("PG_BACKUP_TEST_A", "")
	os.Unsetenv("PG_BACKUP_TEST_A")
	t.Setenv("PG_BACKUP_TEST_B", "")
	os.Unsetenv("PG_BACKUP_TEST_B")

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("PG_BACKUP_TEST_A"); got != "one" {
		t.Errorf("A = %q, want \"one\"", got)
	}
	if got := os.Getenv("PG_BACKUP_TEST_B"); got != "two" {
		t.Errorf("B = %q, want \"two\"", got)
	}
}

// An explicitly set variable must not be clobbered by the file: orchestrators
// and operators outrank a checked-in default.
func TestLoadEnvFileDoesNotOverrideRealEnvironment(t *testing.T) {
	t.Setenv("PG_BACKUP_TEST_PRESET", "from-environment")
	path := writeEnvFile(t, "PG_BACKUP_TEST_PRESET=from-file\n")

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("PG_BACKUP_TEST_PRESET"); got != "from-environment" {
		t.Errorf("value = %q, want the environment to win", got)
	}
}

func TestLoadEnvFileReportsLineNumber(t *testing.T) {
	path := writeEnvFile(t, "GOOD=1\n\nTHIS IS NOT VALID\n")
	err := LoadEnvFile(path)
	if err == nil {
		t.Fatal("expected an error for a malformed line")
	}
	if !strings.Contains(err.Error(), ":3:") {
		t.Errorf("error should point at line 3, got: %v", err)
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

// The shipped example must actually produce a working configuration.
func TestExampleEnvFileIsValid(t *testing.T) {
	body, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	vars := map[string]string{}
	for i, line := range strings.Split(string(body), "\n") {
		key, value, err := parseEnvLine(line)
		if err != nil {
			t.Fatalf(".env.example:%d: %v", i+1, err)
		}
		if key != "" {
			vars[key] = value
		}
	}

	cfg, err := load(lookupFrom(vars))
	if err != nil {
		t.Fatalf(".env.example does not produce a valid configuration: %v", err)
	}
	if cfg.Storage.Type != "local" {
		t.Errorf("example should default to local storage, got %q", cfg.Storage.Type)
	}
	if len(cfg.Database.Databases) == 0 {
		t.Error("example should demonstrate PG_BACKUP_DATABASES")
	}
}
