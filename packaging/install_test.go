// Package packaging tests the installer.
//
// The installer is a shell script that creates a service account, writes a file
// full of credentials and installs a systemd unit, so it is tested by running
// it — in a throwaway container, against a locally built binary, with docker
// and systemctl stubbed out. What is being checked is the part that would be
// expensive to get wrong: file modes, ownership, and that a second run does not
// overwrite a config holding someone's tokens.
package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// image is a plain Debian: the installer must work with nothing but a base
// system, since that is what a fresh VPS is.
const image = "debian:bookworm-slim"

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no docker daemon answered")
	}
}

// runnerBinary cross-compiles the Runner for the container's architecture.
func runnerBinary(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	out := filepath.Join(dir, "roost-runner")

	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/runner")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-compiling the runner: %v: %s", err, combined)
	}
	return dir
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Dir(wd)
}

// inContainer runs script inside the image with the repository mounted at /src
// read-only and binDir at /bin-in.
func inContainer(t *testing.T, binDir, script string) (string, error) {
	t.Helper()

	cmd := exec.Command("docker", "run", "--rm",
		"-v", repoRoot(t)+":/src:ro",
		"-v", binDir+":/bin-in:ro",
		"-w", "/root",
		image, "sh", "-eu", "-c", script,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stubs put a docker and a systemctl on PATH. The installer is right to refuse
// a machine without them; this test is about what it does once they are there.
const stubs = `
mkdir -p /usr/local/bin
printf '#!/bin/sh\nexit 0\n' > /usr/local/bin/docker
printf '#!/bin/sh\necho "systemctl $*" >> /tmp/systemctl.log\nexit 0\n' > /usr/local/bin/systemctl
chmod +x /usr/local/bin/docker /usr/local/bin/systemctl
`

func TestInstallerInstallsEverything(t *testing.T) {
	requireDocker(t)
	binDir := runnerBinary(t)

	script := stubs + `
/src/install.sh \
  --token=rt_installer_test_token \
  --cloud-url=wss://api.example.com/channel \
  --image=alpine:3 \
  --binary=/bin-in/roost-runner

echo "--- binary ---"
/usr/local/bin/roost-runner -version
stat -c '%n %a %U:%G' /usr/local/bin/roost-runner

echo "--- config ---"
stat -c '%n %a %U' /etc/roost/config.json
stat -c '%n %a %U:%G' /etc/roost

echo "--- data ---"
stat -c '%n %a %U' /var/lib/roost

echo "--- account ---"
getent passwd roost | cut -d: -f1,7

echo "--- unit ---"
cat /etc/systemd/system/roost-runner.service

echo "--- systemctl calls ---"
cat /tmp/systemctl.log
`
	out, err := inContainer(t, binDir, script)
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}

	// The config is the file that holds every credential on the box.
	if !strings.Contains(out, "/etc/roost/config.json 600 roost") {
		t.Errorf("the config is not 0600 owned by the service account:\n%s", out)
	}
	// The directory is group-readable by the service account and no wider, so
	// the account can read its config and nobody else can list the directory.
	if !strings.Contains(out, "/etc/roost 750 root:roost") {
		t.Errorf("unexpected config directory mode:\n%s", out)
	}
	if !strings.Contains(out, "/var/lib/roost 700 roost") {
		t.Errorf("the data directory is not private to the service account:\n%s", out)
	}
	if !strings.Contains(out, "/usr/local/bin/roost-runner 755 root:root") {
		t.Errorf("unexpected binary mode:\n%s", out)
	}

	// A service account that can log in is a service account with a shell.
	if !strings.Contains(out, "roost:/usr/sbin/nologin") {
		t.Errorf("the service account has a login shell:\n%s", out)
	}

	for _, want := range []string{
		"User=roost",
		"Requires=docker.service",
		"ReadWritePaths=/var/lib/roost",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"Restart=always",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the unit is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "systemctl enable --now roost-runner") {
		t.Errorf("the service was never enabled:\n%s", out)
	}
}

// The config holds the developer's tokens. Rerunning the installer — to upgrade,
// or because a one-liner was pasted twice — must not wipe them.
func TestInstallerKeepsAnExistingConfig(t *testing.T) {
	requireDocker(t)
	binDir := runnerBinary(t)

	script := stubs + `
/src/install.sh --token=first --cloud-url=wss://api.example.com/channel \
  --binary=/bin-in/roost-runner --no-service >/dev/null

# Stand in for the developer filling in their credentials.
sed -i 's|"git": ""|"git": "ghp_the_developers_token"|' /etc/roost/config.json

/src/install.sh --token=second --cloud-url=wss://elsewhere.example.com/channel \
  --binary=/bin-in/roost-runner --no-service

echo "--- after the second run ---"
cat /etc/roost/config.json
stat -c '%n %a %U' /etc/roost/config.json
`
	out, err := inContainer(t, binDir, script)
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "ghp_the_developers_token") {
		t.Errorf("the second run destroyed the credentials:\n%s", out)
	}
	if strings.Contains(out, `"token": "second"`) {
		t.Errorf("the second run overwrote the config without --force-config:\n%s", out)
	}
	if !strings.Contains(out, "the config was not touched") {
		t.Errorf("the installer overwrote nothing but did not say so:\n%s", out)
	}
	if !strings.Contains(out, "/etc/roost/config.json 600 roost") {
		t.Errorf("the config mode changed:\n%s", out)
	}
}

func TestInstallerRefusesUnsafeInput(t *testing.T) {
	requireDocker(t)
	binDir := runnerBinary(t)

	tests := map[string]struct {
		args string
		want string
	}{
		"no token": {
			args: `--cloud-url=wss://api.example.com/channel`,
			want: "no token",
		},
		"no cloud url": {
			args: `--token=t`,
			want: "no --cloud-url",
		},
		"plain ws to a remote host": {
			args: `--token=t --cloud-url=ws://api.example.com/channel`,
			want: "clear text",
		},
		"not a websocket url": {
			args: `--token=t --cloud-url=https://api.example.com/channel`,
			want: "must be a wss:// URL",
		},
		"unknown option": {
			args: `--token=t --cloud-url=wss://api.example.com/channel --wat`,
			want: "unknown option",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			script := stubs + `
if /src/install.sh ` + tt.args + ` --binary=/bin-in/roost-runner --no-service; then
  echo "INSTALLER ACCEPTED IT"
  exit 1
fi
exit 0
`
			out, err := inContainer(t, binDir, script)
			if err != nil {
				t.Fatalf("the installer accepted bad input: %v\n%s", err, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("error does not mention %q:\n%s", tt.want, out)
			}
			if strings.Contains(out, "Installed.") {
				t.Errorf("the installer reported success anyway:\n%s", out)
			}
		})
	}
}

// Docker is not optional: every task runs in a container and there is no
// fallback, so an installer that quietly succeeded without it would leave a
// machine that fails on its first task instead of at install time.
func TestInstallerRefusesWithoutDocker(t *testing.T) {
	requireDocker(t)
	binDir := runnerBinary(t)

	script := `
mkdir -p /usr/local/bin
printf '#!/bin/sh\nexit 0\n' > /usr/local/bin/systemctl
chmod +x /usr/local/bin/systemctl

if /src/install.sh --token=t --cloud-url=wss://api.example.com/channel \
    --binary=/bin-in/roost-runner --no-service; then
  echo "INSTALLER ACCEPTED IT"
  exit 1
fi
exit 0
`
	out, err := inContainer(t, binDir, script)
	if err != nil {
		t.Fatalf("the installer ran without docker: %v\n%s", err, out)
	}
	if !strings.Contains(out, "docker is not installed") {
		t.Errorf("error does not name the missing prerequisite:\n%s", out)
	}
}

// A dry run is what someone pipes into sh to see what the one-liner would do.
// It must change nothing at all.
func TestDryRunChangesNothing(t *testing.T) {
	requireDocker(t)
	binDir := runnerBinary(t)

	script := stubs + `
/src/install.sh --token=t --cloud-url=wss://api.example.com/channel \
  --binary=/bin-in/roost-runner --dry-run

echo "--- what exists afterwards ---"
test -e /etc/roost && echo "CONFIG DIR EXISTS" || echo "no config dir"
test -e /var/lib/roost && echo "DATA DIR EXISTS" || echo "no data dir"
test -e /usr/local/bin/roost-runner && echo "BINARY EXISTS" || echo "no binary"
getent passwd roost >/dev/null && echo "ACCOUNT EXISTS" || echo "no account"
test -e /tmp/systemctl.log && echo "SYSTEMCTL RAN" || echo "systemctl untouched"
`
	out, err := inContainer(t, binDir, script)
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}

	for _, unwanted := range []string{
		"CONFIG DIR EXISTS", "DATA DIR EXISTS", "BINARY EXISTS",
		"ACCOUNT EXISTS", "SYSTEMCTL RAN",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a dry run changed the machine (%s):\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "would run") {
		t.Errorf("a dry run printed no plan:\n%s", out)
	}
}
