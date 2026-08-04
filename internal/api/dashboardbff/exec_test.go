package dashboardbff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCleanBdEnvFallsBackToSupervisorDoltCredentials(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_USER", "")
	t.Setenv("BEADS_DOLT_PASSWORD", "")
	t.Setenv("GC_DOLT_USER", "supervisor-reader")
	t.Setenv("GC_DOLT_PASSWORD", "supervisor-password")

	env := envMap(cleanBdEnv())
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "supervisor-reader" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want supervisor fallback", got)
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "supervisor-password" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want supervisor fallback", got)
	}
	for _, key := range []string{"GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		if _, ok := env[key]; ok {
			t.Fatalf("cleanBdEnv() leaked supervisor credential %s", key)
		}
	}
}

func TestCleanBdEnvPrefersExplicitBeadsDoltCredentials(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_USER", "health-reader")
	t.Setenv("BEADS_DOLT_PASSWORD", "test-password")
	t.Setenv("GC_DOLT_USER", "supervisor-reader")
	t.Setenv("GC_DOLT_PASSWORD", "supervisor-password")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")

	env := envMap(cleanBdEnv())
	if got := env["BEADS_DOLT_SERVER_USER"]; got != "health-reader" {
		t.Fatalf("BEADS_DOLT_SERVER_USER = %q, want explicit beads credential", got)
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "test-password" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want explicit beads credential", got)
	}
	if _, ok := env["GITHUB_TOKEN"]; ok {
		t.Fatal("cleanBdEnv() leaked GITHUB_TOKEN")
	}
	for _, key := range []string{"GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		if _, ok := env[key]; ok {
			t.Fatalf("cleanBdEnv() leaked supervisor credential %s", key)
		}
	}
}

func TestCleanEnvExcludesDoltCredentialsFromGenericProbes(t *testing.T) {
	for key, value := range map[string]string{
		"BEADS_DOLT_SERVER_USER": "health-reader",
		"BEADS_DOLT_PASSWORD":    "beads-password",
		"GC_DOLT_USER":           "supervisor-reader",
		"GC_DOLT_PASSWORD":       "supervisor-password",
	} {
		t.Setenv(key, value)
	}

	env := envMap(cleanEnv())
	for _, key := range []string{
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
	} {
		if _, ok := env[key]; ok {
			t.Fatalf("cleanEnv() leaked Dolt credential %s to generic probes", key)
		}
	}
}

func envMap(env []string) map[string]string {
	values := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func TestExecBdDoctorRejectsBeadsDirectoryAsRigPath(t *testing.T) {
	r := newExecRunner()
	for _, path := range []string{"/tmp/example/.beads", "/tmp/example/.beads/"} {
		t.Run(path, func(t *testing.T) {
			_, err := r.execBdDoctor(t.Context(), path)
			if err == nil || !strings.Contains(err.Error(), "invalid rig path") {
				t.Fatalf("execBdDoctor(%q) error = %v, want invalid rig path", path, err)
			}
		})
	}
}

func TestExecBdDoctorUsesRigContextAndScopedCredentials(t *testing.T) {
	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	const helper = `#!/bin/sh
printf '{"args":["%s","%s","%s","%s","%s"],"user":"%s","password":"%s","github":"%s"}\n' \
  "$1" "$2" "$3" "$4" "$5" \
  "${BEADS_DOLT_SERVER_USER-}" "${BEADS_DOLT_PASSWORD-}" "${GITHUB_TOKEN-}"
`
	if err := os.WriteFile(bdPath, []byte(helper), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	t.Setenv("PATH", binDir)
	t.Setenv("ADMIN_PATH", binDir)
	t.Setenv("BEADS_DOLT_SERVER_USER", "health-reader")
	t.Setenv("BEADS_DOLT_PASSWORD", "test-password")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")

	rigPath := filepath.Join(t.TempDir(), "rig")
	result, err := newExecRunner().execBdDoctor(t.Context(), rigPath)
	if err != nil {
		t.Fatalf("execBdDoctor: %v", err)
	}
	if result.exitCode != 0 {
		t.Fatalf("execBdDoctor exit code = %d, stderr = %q", result.exitCode, result.stderr)
	}

	var got struct {
		Args     []string `json:"args"`
		User     string   `json:"user"`
		Password string   `json:"password"`
		GitHub   string   `json:"github"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &got); err != nil {
		t.Fatalf("decode fake bd output %q: %v", result.stdout, err)
	}
	wantArgs := []string{"doctor", "--readonly", "--json", "-C", rigPath}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Fatalf("bd args = %q, want %q", got.Args, wantArgs)
	}
	if got.User != "health-reader" || got.Password != "test-password" {
		t.Fatalf("bd credentials = (%q, %q), want scoped Dolt credentials", got.User, got.Password)
	}
	if got.GitHub != "" {
		t.Fatalf("bd inherited GITHUB_TOKEN = %q, want empty", got.GitHub)
	}
}
