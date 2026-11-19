// Package prod_test exercises deploy.sh with a fake docker command.
package prod_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	image  = "ghcr.io/cembasaranoglu/trader@sha256:" + digest
)

// fakeDocker records its arguments to $FAKE_LOG, prints a container id for
// "create", writes the destination of "cp", and fails when its arguments
// contain $FAKE_FAIL.
const fakeDocker = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG"
if [ -n "${FAKE_FAIL:-}" ]; then
  case "$*" in *"$FAKE_FAIL"*) echo "fake failure: $*" >&2; exit 1 ;; esac
fi
case "$1" in
  login) cat > "$DOCKER_CONFIG/token-seen" ;;
  create) echo cid123 ;;
  cp) printf 'name: trader\n# new\n' > "$3" ;;
esac
exit 0
`

type run struct {
	code   int
	stderr string
	log    string
	base   string
}

func deploy(t *testing.T, command, stdin, fail string) run {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	base := filepath.Join(dir, "opt")
	for _, d := range []string{bin, base} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"compose.yaml": "name: trader\n# old\n", ".env": "TRADER_IMAGE=old\n"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logf := filepath.Join(dir, "docker.log")
	cmd := exec.Command("sh", "deploy.sh")
	cmd.Env = []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"SSH_ORIGINAL_COMMAND=" + command,
		"TRADER_DEPLOY_BASE=" + base,
		"FAKE_LOG=" + logf,
		"FAKE_FAIL=" + fail,
		"TMPDIR=" + dir,
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(logf)
	return run{code: code, stderr: stderr.String(), log: string(b), base: base}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeploySucceeds(t *testing.T) {
	r := deploy(t, "deploy "+image, "octocat\ntoken-123\n", "")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	want := []string{
		"login ghcr.io -u octocat --password-stdin",
		"pull --quiet " + image,
		"logout ghcr.io",
		"create " + image,
		"cp cid123:/deploy/compose.yaml " + filepath.Join(r.base, "compose.yaml.new"),
		"rm cid123",
		"compose -f compose.yaml.new run --rm migrate",
		"compose -f compose.yaml.new run --rm verify",
		"compose up -d --remove-orphans --wait",
		"compose ps",
	}
	if got := strings.Split(strings.TrimSpace(r.log), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("docker calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := read(t, filepath.Join(r.base, ".env")); got != "TRADER_IMAGE="+image+"\n" {
		t.Fatalf(".env %q", got)
	}
	if !strings.Contains(read(t, filepath.Join(r.base, "compose.yaml")), "# new") {
		t.Fatal("compose.yaml not replaced")
	}
	if !strings.HasSuffix(strings.TrimSpace(read(t, filepath.Join(r.base, "deployments.log"))), " "+image) {
		t.Fatal("deployment not logged")
	}
	if _, err := os.Stat(filepath.Join(r.base, "compose.yaml.new")); !os.IsNotExist(err) {
		t.Fatal("compose.yaml.new left behind")
	}
	// The temporary Docker config (which saw the token) is gone.
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(r.base), "tmp.*", "token-seen"))
	if len(matches) != 0 {
		t.Fatalf("registry credentials left on disk: %v", matches)
	}
}

func TestDeployFailureLeavesServicesAndFilesUnchanged(t *testing.T) {
	for _, step := range []string{"run --rm migrate", "run --rm verify", "pull --quiet"} {
		t.Run(step, func(t *testing.T) {
			r := deploy(t, "deploy "+image, "octocat\ntoken-123\n", step)
			if r.code == 0 {
				t.Fatal("deploy succeeded despite a failing step")
			}
			if strings.Contains(r.log, "compose up") {
				t.Fatal("services were restarted after a failed step")
			}
			if !strings.Contains(read(t, filepath.Join(r.base, "compose.yaml")), "# old") {
				t.Fatal("compose.yaml replaced after a failed step")
			}
			if read(t, filepath.Join(r.base, ".env")) != "TRADER_IMAGE=old\n" {
				t.Fatal(".env changed after a failed step")
			}
			if _, err := os.Stat(filepath.Join(r.base, "compose.yaml.new")); !os.IsNotExist(err) {
				t.Fatal("compose.yaml.new left behind")
			}
		})
	}
}

func TestDeployRejectsBadInput(t *testing.T) {
	cases := map[string]struct{ command, stdin string }{
		"no command":       {"", "u\nt\n"},
		"shell":            {"bash -i", "u\nt\n"},
		"other repository": {"deploy ghcr.io/someone/else@sha256:" + digest, "u\nt\n"},
		"tag not digest":   {"deploy ghcr.io/cembasaranoglu/trader:latest", "u\nt\n"},
		"short digest":     {"deploy ghcr.io/cembasaranoglu/trader@sha256:abc", "u\nt\n"},
		"uppercase digest": {"deploy ghcr.io/cembasaranoglu/trader@sha256:" + strings.ToUpper(digest), "u\nt\n"},
		"newline suffix":   {"deploy " + image + "\n--evil", "u\nt\n"},
		"space suffix":     {"deploy " + image + " ; rm -rf /", "u\nt\n"},
		"missing token":    {"deploy " + image, "u\n"},
		"empty token":      {"deploy " + image, "u\n\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := deploy(t, c.command, c.stdin, "")
			if r.code != 2 {
				t.Fatalf("exit %d (want 2), stderr %q", r.code, r.stderr)
			}
			if strings.Contains(r.log, "pull") || strings.Contains(r.log, "compose") {
				t.Fatalf("docker ran: %s", r.log)
			}
		})
	}
}
