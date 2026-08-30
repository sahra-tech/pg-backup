package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile reads KEY=value lines from path into the process environment.
//
// Variables already present in the environment win, so a file cannot silently
// override what an operator or orchestrator set explicitly. Under Docker,
// compose's own env_file makes this unnecessary; it exists so ad-hoc CLI runs
// stay as convenient as the old -config flag was.
//
// Supported: blank lines, # comments, an optional "export " prefix, and values
// wrapped in single or double quotes. Escapes inside double quotes (\n, \", \\)
// are interpreted; single-quoted values are taken literally.
func LoadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to read env file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		key, value, err := parseEnvLine(scanner.Text())
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if key == "" {
			continue
		}
		if _, present := os.LookupEnv(key); present {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read env file: %w", err)
	}
	return nil
}

// parseEnvLine returns an empty key for lines that carry no assignment.
func parseEnvLine(line string) (key, value string, err error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", nil
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")

	name, raw, found := strings.Cut(trimmed, "=")
	if !found {
		return "", "", fmt.Errorf("expected KEY=value, got %q", line)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fmt.Errorf("missing variable name in %q", line)
	}

	raw = strings.TrimSpace(raw)
	switch {
	case len(raw) >= 2 && strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`):
		unquoted := raw[1 : len(raw)-1]
		replacer := strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`)
		return name, replacer.Replace(unquoted), nil
	case len(raw) >= 2 && strings.HasPrefix(raw, `'`) && strings.HasSuffix(raw, `'`):
		return name, raw[1 : len(raw)-1], nil
	default:
		// Trailing comments are only stripped on unquoted values, and only when
		// the # is separated by whitespace -- passwords may legitimately
		// contain '#'.
		if idx := strings.Index(raw, " #"); idx >= 0 {
			raw = strings.TrimSpace(raw[:idx])
		}
		return name, raw, nil
	}
}
