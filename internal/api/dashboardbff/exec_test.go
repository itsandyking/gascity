package dashboardbff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCleanBdEnvScopesDoltCredentialsToBdProbe(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_USER", "health-reader")
	t.Setenv("BEADS_DOLT_PASSWORD", "test-password")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")

	env := cleanBdEnv()
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"\nBEADS_DOLT_SERVER_USER=health-reader\n",
		"\nBEADS_DOLT_PASSWORD=test-password\n",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cleanBdEnv() missing %q", strings.TrimSpace(want))
		}
	}
	if strings.Contains(joined, "GITHUB_TOKEN=") {
		t.Fatal("cleanBdEnv() leaked GITHUB_TOKEN")
	}
	if strings.Contains("\n"+strings.Join(cleanEnv(), "\n")+"\n", "BEADS_DOLT_PASSWORD=") {
		t.Fatal("cleanEnv() leaked Dolt credentials to non-bd probes")
	}
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
