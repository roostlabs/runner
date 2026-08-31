#!/bin/sh
# Roost Runner installer.
#
# Installs the Runner binary, writes its configuration, and runs it as a systemd
# service. Meant to be piped from the dashboard's one-liner:
#
#   curl -fsSL https://raw.githubusercontent.com/roostlabs/runner/main/install.sh \
#     | sh -s -- --token=<from the dashboard> --cloud-url=wss://<cloud>/channel
#
# It is deliberately boring: POSIX sh, no dependencies beyond curl or wget, and
# it refuses rather than guesses. Run it with --dry-run to see every step it
# would take without touching the machine.
set -eu

REPO=roostlabs/runner
BIN_NAME=roost-runner

PREFIX=/usr/local
CONFIG_DIR=/etc/roost
DATA_DIR=/var/lib/roost
SERVICE_USER=roost
SERVICE_NAME=roost-runner
UNIT_DIR=/etc/systemd/system

VERSION=latest
TOKEN=${ROOST_TOKEN:-}
TOKEN_FILE=
CLOUD_URL=
IMAGE=
BINARY=
INSTALL_SERVICE=yes
DRY_RUN=no
FORCE_CONFIG=no

usage() {
	cat <<'USAGE'
Usage: install.sh --token=<token> --cloud-url=<wss://host/channel> [options]

Required:
  --token=<token>          Runner token from the dashboard. ROOST_TOKEN also works.
  --token-file=<path>      Read the token from a file instead, so it never
                           appears in the process list.
  --cloud-url=<url>        Channel endpoint, e.g. wss://api.example.com/channel.

Options:
  --image=<image>          Container image tasks run in, e.g. golang:1.26. The
                           Runner ships none: which image a task needs is a
                           property of the repository being worked on.
  --version=<tag>          Release to install. Default: latest.
  --binary=<path>          Install this binary instead of downloading one.
  --prefix=<dir>           Install prefix. Default: /usr/local.
  --config-dir=<dir>       Default: /etc/roost.
  --data-dir=<dir>         Default: /var/lib/roost.
  --user=<name>            Service account to run as. Default: roost.
  --force-config           Overwrite an existing config. Off by default: that
                           file holds every credential on the box.
  --no-service             Install the files, do not touch systemd.
  --dry-run                Print what would happen and change nothing.
  -h, --help               This.
USAGE
}

log() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

# run executes a command, or prints it under --dry-run.
run() {
	if [ "$DRY_RUN" = yes ]; then
		printf '  would run: %s\n' "$*"
		return 0
	fi
	"$@"
}

for arg in "$@"; do
	case $arg in
	--token=*) TOKEN=${arg#*=} ;;
	--token-file=*) TOKEN_FILE=${arg#*=} ;;
	--cloud-url=*) CLOUD_URL=${arg#*=} ;;
	--image=*) IMAGE=${arg#*=} ;;
	--version=*) VERSION=${arg#*=} ;;
	--binary=*) BINARY=${arg#*=} ;;
	--prefix=*) PREFIX=${arg#*=} ;;
	--config-dir=*) CONFIG_DIR=${arg#*=} ;;
	--data-dir=*) DATA_DIR=${arg#*=} ;;
	--user=*) SERVICE_USER=${arg#*=} ;;
	--force-config) FORCE_CONFIG=yes ;;
	--no-service) INSTALL_SERVICE=no ;;
	--dry-run) DRY_RUN=yes ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown option $arg (try --help)" ;;
	esac
done

BIN_DIR=$PREFIX/bin
CONFIG_FILE=$CONFIG_DIR/config.json
UNIT_FILE=$UNIT_DIR/$SERVICE_NAME.service

if [ -n "$TOKEN_FILE" ]; then
	[ -r "$TOKEN_FILE" ] || die "cannot read the token file $TOKEN_FILE"
	TOKEN=$(cat "$TOKEN_FILE")
fi

# --- checks, before anything is changed -------------------------------------

case $(uname -s) in
Linux) ;;
*)
	die "this installer targets Linux with systemd; $(uname -s) is not supported.
Build and run the Runner directly instead: go build ./cmd/runner"
	;;
esac

case $(uname -m) in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported architecture $(uname -m); only amd64 and arm64 are released" ;;
esac

if [ "$DRY_RUN" = no ] && [ "$(id -u)" != 0 ]; then
	die "run this as root: it creates a service account and installs a systemd unit"
fi

[ -n "$TOKEN" ] || die "no token; pass --token=<token>, --token-file=<path>, or set ROOST_TOKEN"
[ -n "$CLOUD_URL" ] || die "no --cloud-url; the dashboard shows the channel endpoint to use"

case $CLOUD_URL in
wss://*) ;;
ws://127.0.0.1* | ws://localhost*)
	warn "$CLOUD_URL is unencrypted; that is for a local cloud only"
	;;
ws://*)
	die "$CLOUD_URL uses ws:// to a remote host; the token would cross the network in clear text"
	;;
*) die "--cloud-url must be a wss:// URL, got $CLOUD_URL" ;;
esac

if ! have docker; then
	die "docker is not installed. The Runner executes every task in a container
and has no fallback, by design. Install Docker and run this again."
fi
if ! docker info >/dev/null 2>&1; then
	warn "the docker daemon did not answer. The service will start, but tasks
         will fail until docker is running."
fi

if [ "$INSTALL_SERVICE" = yes ] && ! have systemctl; then
	die "systemctl not found. Use --no-service to install the files and run the
Runner yourself."
fi

if have getent && ! getent group docker >/dev/null 2>&1; then
	warn "there is no docker group. If your Docker needs one, $SERVICE_USER will
         not reach the daemon socket."
fi

# --- the binary -------------------------------------------------------------

TMP_DIR=
# cleanup must succeed even with nothing to clean: this runs from an EXIT trap,
# and under set -e a trap whose last command fails becomes the script's exit
# status. An installer that reported failure after a successful install would
# break every `install.sh && ...` and every check the dashboard's one-liner does.
# shellcheck disable=SC2329 # invoked from the trap below
cleanup() {
	if [ -n "$TMP_DIR" ]; then
		rm -rf "$TMP_DIR"
	fi
	return 0
}
trap cleanup EXIT INT TERM

fetch() {
	url=$1
	out=$2
	if have curl; then
		curl -fsSL "$url" -o "$out"
	elif have wget; then
		wget -qO "$out" "$url"
	else
		die "neither curl nor wget is installed"
	fi
}

checksum() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		echo ""
	fi
}

if [ -n "$BINARY" ]; then
	[ -f "$BINARY" ] || die "no such file: $BINARY"
	log "installing the binary at $BINARY"
	SOURCE_BINARY=$BINARY
elif [ "$DRY_RUN" = yes ]; then
	log "would download $BIN_NAME $VERSION for linux/$ARCH"
	SOURCE_BINARY=
else
	TMP_DIR=$(mktemp -d)
	asset="${BIN_NAME}_linux_${ARCH}.tar.gz"
	if [ "$VERSION" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		base="https://github.com/$REPO/releases/download/$VERSION"
	fi

	log "downloading $asset ($VERSION)"
	fetch "$base/$asset" "$TMP_DIR/$asset" || die "could not download $base/$asset.
The releases may not be published yet. Build it yourself and pass --binary:
  git clone https://github.com/$REPO && cd runner && make build"

	# A checksum is only worth something if it is there; a missing one is
	# reported rather than skipped silently.
	if fetch "$base/$asset.sha256" "$TMP_DIR/$asset.sha256" 2>/dev/null; then
		want=$(awk '{print $1}' "$TMP_DIR/$asset.sha256")
		got=$(checksum "$TMP_DIR/$asset")
		[ -n "$got" ] || die "cannot verify the download: no sha256sum or shasum"
		[ "$want" = "$got" ] || die "checksum mismatch for $asset
  expected $want
  got      $got"
		log "checksum verified"
	else
		warn "no published checksum for $asset; the download was not verified"
	fi

	tar -xzf "$TMP_DIR/$asset" -C "$TMP_DIR"
	SOURCE_BINARY=$TMP_DIR/$BIN_NAME
	[ -f "$SOURCE_BINARY" ] || die "the archive did not contain $BIN_NAME"
fi

# --- the service account ----------------------------------------------------

if have getent && getent passwd "$SERVICE_USER" >/dev/null 2>&1; then
	log "service account $SERVICE_USER already exists"
elif [ "$SERVICE_USER" = root ]; then
	warn "running the Runner as root; a dedicated account is safer"
else
	log "creating the service account $SERVICE_USER"
	run useradd --system --no-create-home --home-dir "$DATA_DIR" \
		--shell /usr/sbin/nologin "$SERVICE_USER"
fi

if [ "$SERVICE_USER" != root ] && have getent && getent group docker >/dev/null 2>&1; then
	log "adding $SERVICE_USER to the docker group"
	run usermod -aG docker "$SERVICE_USER"
fi

# --- files ------------------------------------------------------------------

log "installing $BIN_NAME to $BIN_DIR"
run install -d -m 0755 "$BIN_DIR"
if [ -n "$SOURCE_BINARY" ]; then
	run install -m 0755 "$SOURCE_BINARY" "$BIN_DIR/$BIN_NAME"
else
	printf '  would install: %s\n' "$BIN_DIR/$BIN_NAME"
fi

log "creating $DATA_DIR"
run install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DATA_DIR"

log "creating $CONFIG_DIR"
run install -d -m 0750 -o root -g "$SERVICE_USER" "$CONFIG_DIR"

write_config() {
	# The file holds every credential on the box, so it is written through a
	# tight umask and chmodded explicitly: install(1)'s mode is applied after
	# the content is already on disk, and WriteFile-style modes go through the
	# umask.
	tmp="$CONFIG_FILE.tmp"
	(
		umask 077
		cat >"$tmp" <<CONFIG
{
  "cloudUrl": "$CLOUD_URL",
  "token": "$TOKEN",
  "dataDir": "$DATA_DIR",
  "creds": {
    "git": "",
    "taskManager": "",
    "llm": ""
  },
  "sandbox": {
    "image": "$IMAGE",
    "network": "bridge"
  },
  "agent": {
    "effort": "xhigh",
    "budgetUsd": 5
  }
}
CONFIG
	)
	chmod 0600 "$tmp"
	chown "$SERVICE_USER" "$tmp"
	mv "$tmp" "$CONFIG_FILE"
}

if [ -f "$CONFIG_FILE" ] && [ "$FORCE_CONFIG" = no ]; then
	log "keeping the existing $CONFIG_FILE"
	warn "the config was not touched, so the token you passed was not written.
         Pass --force-config to replace it, or edit the file."
elif [ "$DRY_RUN" = yes ]; then
	printf '  would write: %s (mode 0600, owner %s)\n' "$CONFIG_FILE" "$SERVICE_USER"
else
	log "writing $CONFIG_FILE"
	write_config
fi

# --- the service ------------------------------------------------------------

write_unit() {
	cat >"$UNIT_FILE" <<UNIT
[Unit]
Description=Roost Runner
Documentation=https://github.com/$REPO
# The Runner dials out; nothing dials in. It needs the network up and the docker
# daemon reachable, because a task with no container has nowhere to run.
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
User=$SERVICE_USER
ExecStart=$BIN_DIR/$BIN_NAME -config $CONFIG_FILE
Restart=always
RestartSec=5s

# A dropped channel is normal and the Runner reconnects on its own, so a restart
# here is for a crash, not for a network blip.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=$DATA_DIR
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes

StandardOutput=journal
StandardError=journal
SyslogIdentifier=$SERVICE_NAME

[Install]
WantedBy=multi-user.target
UNIT
	chmod 0644 "$UNIT_FILE"
}

if [ "$INSTALL_SERVICE" = no ]; then
	log "skipping systemd (--no-service)"
elif [ "$DRY_RUN" = yes ]; then
	printf '  would write: %s\n' "$UNIT_FILE"
	printf '  would run: systemctl daemon-reload\n'
	printf '  would run: systemctl enable --now %s\n' "$SERVICE_NAME"
else
	log "writing $UNIT_FILE"
	write_unit
	log "starting $SERVICE_NAME"
	systemctl daemon-reload
	systemctl enable --now "$SERVICE_NAME"
fi

# --- what to do next --------------------------------------------------------

cat <<DONE

Installed.

  binary   $BIN_DIR/$BIN_NAME
  config   $CONFIG_FILE (mode 0600, owner $SERVICE_USER)
  data     $DATA_DIR
DONE

if [ "$INSTALL_SERVICE" = yes ] && [ "$DRY_RUN" = no ]; then
	cat <<DONE
  service  $SERVICE_NAME

    systemctl status $SERVICE_NAME
    journalctl -u $SERVICE_NAME -f
DONE
fi

cat <<'DONE'

Before a task can run, put the credentials in the config file. They stay on this
machine: the cloud is told which ones exist, never what they are.

  git          a service account token that can push and open pull requests,
               and should not be able to merge one
  llm          your model provider API key
  taskManager  optional

DONE

if [ -z "$IMAGE" ]; then
	cat <<'DONE'
sandbox.image is empty, so tasks will fail until it is set. There is no default
on purpose: which image a task needs is a property of the repository, not of the
Runner.

DONE
fi

if [ "$INSTALL_SERVICE" = yes ] && [ "$DRY_RUN" = no ]; then
	printf 'Then: systemctl restart %s\n' "$SERVICE_NAME"
fi

exit 0
