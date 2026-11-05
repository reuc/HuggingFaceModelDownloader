#!/bin/sh
# =============================================================================
# hfdownloader container entrypoint
# =============================================================================
# Environment variables:
#   HTTP_PORT  Port for `serve` (same as --port). Default 8080.
#   AUTH_USER  Basic-auth username for `serve` (same as --auth-user).
#              Also the SSH login name when SSH_PORT is set.
#   AUTH_PASS  Basic-auth password for `serve` (same as --auth-pass).
#              Also the SSH password when SSH_PORT is set.
#   SSH_ENABLE Start an SSH/SFTP server so you can scp/sftp models in and out
#              or run hfdownloader remotely. Boolean (true/false, 1/0,
#              yes/no, on/off). Default false: SSH is off unless enabled.
#              Requires AUTH_USER and AUTH_PASS, and the container must
#              start as root (the image default).
#   SSH_PORT   Port for the SSH server when SSH_ENABLE is true. Default 22.
#   FLATTEN    When a download completes, move its files into models/ (or
#              datasets/) as real files and remove it from hub/ (same as
#              `serve --flatten`). Boolean like SSH_ENABLE. When set (true or
#              false) it overrides `flatten` in the config file; unset leaves
#              the config file's value (default false).
#   CONVERT_ON_BOOT
#              At startup, convert every completed download already in hub/
#              to real files in models/ (or datasets/), like
#              `hfdownloader flatten --all` (same as `serve --convert-on-boot`).
#              Use with FLATTEN=true. Boolean, set/unset like FLATTEN.
#
# Flags passed explicitly on the command line always win over these variables.
# =============================================================================
set -eu

APP_USER=hfdownloader
APP_HOME=/home/hfdownloader
BIN=/usr/local/bin/hfdownloader

die() { echo "docker-entrypoint: $*" >&2; exit 1; }

check_port() {
	case "$2" in
		'' | *[!0-9]*) die "$1 must be a port number, got '$2'" ;;
	esac
	[ "$2" -ge 1 ] && [ "$2" -le 65535 ] || die "$1 must be between 1 and 65535, got '$2'"
}

AUTH_USER="${AUTH_USER:-}"
AUTH_PASS="${AUTH_PASS:-}"
HTTP_PORT="${HTTP_PORT:-}"
SSH_PORT="${SSH_PORT:-}"
SSH_ENABLE="${SSH_ENABLE:-false}"
FLATTEN="${FLATTEN:-}"
CONVERT_ON_BOOT="${CONVERT_ON_BOOT:-}"

# parse_bool <NAME> <value>: prints true or false, or dies.
parse_bool() {
	case "$(echo "$2" | tr '[:upper:]' '[:lower:]')" in
		1 | true | yes | on) echo true ;;
		0 | false | no | off | '') echo false ;;
		*) die "$1 must be true or false, got '$2'" ;;
	esac
}
SSH_ENABLE=$(parse_bool SSH_ENABLE "$SSH_ENABLE")
[ -n "$FLATTEN" ] && FLATTEN=$(parse_bool FLATTEN "$FLATTEN")
[ -n "$CONVERT_ON_BOOT" ] && CONVERT_ON_BOOT=$(parse_bool CONVERT_ON_BOOT "$CONVERT_ON_BOOT")

if [ -n "$AUTH_USER" ] && [ -z "$AUTH_PASS" ]; then die "AUTH_USER is set but AUTH_PASS is empty"; fi
if [ -z "$AUTH_USER" ] && [ -n "$AUTH_PASS" ]; then die "AUTH_PASS is set but AUTH_USER is empty"; fi
[ -n "$HTTP_PORT" ] && check_port HTTP_PORT "$HTTP_PORT"

# -----------------------------------------------------------------------------
# Optional SSH server
# -----------------------------------------------------------------------------
start_sshd() {
	SSH_PORT="${SSH_PORT:-22}"
	check_port SSH_PORT "$SSH_PORT"
	[ -n "$AUTH_USER" ] || die "SSH_ENABLE requires AUTH_USER and AUTH_PASS"
	[ "$(id -u)" = 0 ] || die "SSH_ENABLE requires the container to start as root (do not pass --user)"
	echo "$AUTH_USER" | grep -Eq '^[a-z_][a-z0-9_-]{0,31}$' ||
		die "AUTH_USER must be a valid login name for SSH (lowercase letters, digits, _ and -)"
	[ "$AUTH_USER" != root ] || die "AUTH_USER must not be root"
	nl='
'
	case "$AUTH_PASS" in
		*"$nl"*) die "AUTH_PASS must not contain newlines" ;;
	esac

	# The SSH login is an alias of the app user (same uid/gid/home), so files
	# copied in over SSH are owned by the user that runs hfdownloader.
	if ! grep -q "^${AUTH_USER}:" /etc/passwd; then
		echo "${AUTH_USER}:x:1000:1000:hfdownloader:${APP_HOME}:/bin/sh" >>/etc/passwd
		echo "${AUTH_USER}:!:0:0:99999:7:::" >>/etc/shadow
	fi
	[ "$(id -u "$AUTH_USER")" = 1000 ] || die "AUTH_USER '$AUTH_USER' clashes with an existing system account"
	printf '%s:%s\n' "$AUTH_USER" "$AUTH_PASS" | chpasswd >/dev/null 2>&1 || die "could not set the SSH password"

	# Host keys live in /etc/ssh/keys; mount a volume there to keep them stable.
	mkdir -p /etc/ssh/keys
	for t in ed25519 rsa; do
		[ -f "/etc/ssh/keys/ssh_host_${t}_key" ] ||
			ssh-keygen -q -t "$t" -N '' -f "/etc/ssh/keys/ssh_host_${t}_key"
	done

	cat >/etc/ssh/sshd_config.d/hfdownloader.conf <<CONF
Port ${SSH_PORT}
HostKey /etc/ssh/keys/ssh_host_ed25519_key
HostKey /etc/ssh/keys/ssh_host_rsa_key
PermitRootLogin no
PasswordAuthentication yes
KbdInteractiveAuthentication no
AllowUsers ${AUTH_USER}
AllowTcpForwarding no
X11Forwarding no
SetEnv HF_HOME=${HF_HOME:-${APP_HOME}/.cache/huggingface}
Subsystem sftp internal-sftp
CONF

	/usr/sbin/sshd -t || die "sshd configuration is invalid"
	# -D -e keeps sshd attached so its log goes to the container log.
	/usr/sbin/sshd -D -e &
	echo "docker-entrypoint: SSH server listening on port ${SSH_PORT} (user ${AUTH_USER})" >&2
}

if [ "$SSH_ENABLE" = true ]; then
	start_sshd
elif [ -n "$SSH_PORT" ]; then
	echo "docker-entrypoint: SSH_PORT is set but SSH_ENABLE is not true; SSH stays off" >&2
fi

# -----------------------------------------------------------------------------
# Translate env vars into `serve` flags (explicit flags win)
# -----------------------------------------------------------------------------
has_flag() {
	# has_flag <long> [short] -- "$@"
	long=$1 short=$2
	shift 3
	for a in "$@"; do
		case "$a" in
			--) return 1 ;;
			"$long" | "$long"=*) return 0 ;;
		esac
		if [ -n "$short" ]; then
			case "$a" in "$short" | "$short"*) return 0 ;; esac
		fi
	done
	return 1
}

if [ "${1:-}" = serve ]; then
	shift
	if [ -n "$HTTP_PORT" ] && ! has_flag --port -p -- "$@"; then
		set -- --port "$HTTP_PORT" "$@"
	fi
	if [ -n "$AUTH_USER" ] && ! has_flag --auth-user "" -- "$@" && ! has_flag --auth-pass "" -- "$@"; then
		set -- --auth-user "$AUTH_USER" --auth-pass "$AUTH_PASS" "$@"
	fi
	if [ -n "$FLATTEN" ] && ! has_flag --flatten "" -- "$@"; then
		set -- "--flatten=$FLATTEN" "$@"
	fi
	if [ -n "$CONVERT_ON_BOOT" ] && ! has_flag --convert-on-boot "" -- "$@"; then
		set -- "--convert-on-boot=$CONVERT_ON_BOOT" "$@"
	fi
	set -- serve "$@"
fi

# Drop root before running the app.
if [ "$(id -u)" = 0 ]; then
	exec su-exec "$APP_USER" "$BIN" "$@"
fi
exec "$BIN" "$@"
