#!/usr/bin/env bash
# =============================================================================
# Smoke test for the hfdownloader container image
# =============================================================================
# Usage:
#   scripts/docker-smoke-test.sh IMAGE
#
# Checks the CLI, the image hardening, the entrypoint's env-var handling and
# privilege drop, running as a custom uid, and SSH/SFTP (SSH_ENABLE=true).
# Needs docker and curl. Doesn't contact Hugging Face; the SSH login test
# pulls cgr.dev/chainguard/wolfi-base to run an ssh/sftp client.
# =============================================================================
set -euo pipefail

image=${1:?usage: $0 IMAGE}
prefix="hfd-smoke-$$"
client_image=cgr.dev/chainguard/wolfi-base:latest
pass_word='smoke pass' # with a space, to check quoting in the entrypoint

containers=()
cleanup() {
	for c in "${containers[@]}"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
	docker network rm "$prefix-net" >/dev/null 2>&1 || true
}
trap cleanup EXIT

ok() { echo "ok   - $*"; }
fail() {
	echo "FAIL - $*" >&2
	for c in "${containers[@]}"; do
		echo "---- docker logs $c" >&2
		docker logs "$c" >&2 2>&1 || true
	done
	exit 1
}

start() { # start NAME [docker run args...] -- runs detached, records for cleanup
	local name=$1
	shift
	containers+=("$name")
	docker run -d --name "$name" "$@" >/dev/null
}

host_port() { # host_port CONTAINER CONTAINER_PORT
	docker port "$1" "$2/tcp" | head -1 | sed 's/.*://'
}

wait_http() { # wait_http URL -- until the server answers (any HTTP status)
	for _ in $(seq 1 30); do
		[ "$(curl -s -o /dev/null -w '%{http_code}' "$1")" != 000 ] && return 0
		sleep 1
	done
	return 1
}

http_code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# ---- CLI ---------------------------------------------------------------------
out=$(docker run --rm "$image" version 2>&1) || fail "version exited non-zero: $out"
grep -q '^hfdownloader ' <<<"$out" || fail "version output unexpected: $out"
ok "hfdownloader version ($(head -1 <<<"$out"))"

out=$(docker run --rm "$image" 2>&1) || fail "default command exited non-zero"
grep -q 'Usage:' <<<"$out" || fail "default command doesn't print help"
ok "default command prints help"

# ---- Hardening ---------------------------------------------------------------
out=$(docker run --rm --entrypoint /bin/sh "$image" -c '
	for f in /sbin/apk /usr/bin/apk; do [ ! -e "$f" ] || echo "$f present"; done
	s=$(find / -xdev -type f -perm /6000 2>/dev/null); [ -z "$s" ] || echo "setuid/setgid: $s"
	[ "$(stat -c %a /etc/shadow)" = 600 ] || echo "/etc/shadow mode is $(stat -c %a /etc/shadow)"
	[ "$(id -u hfdownloader)" = 1000 ] || echo "hfdownloader uid is $(id -u hfdownloader)"
' 2>&1) || fail "hardening check couldn't run: $out"
[ -z "$out" ] || fail "hardening: $out"
ok "no package manager, no setuid/setgid files, /etc/shadow 0600, app user uid 1000"

# ---- serve: env vars -> flags, basic auth, privilege drop ---------------------
start "$prefix-serve" -p 127.0.0.1::3000 \
	-e HTTP_PORT=3000 -e AUTH_USER=smoke -e AUTH_PASS="$pass_word" \
	-e FLATTEN=true -e CONVERT_ON_BOOT=yes \
	"$image" serve
port=$(host_port "$prefix-serve" 3000)
wait_http "http://127.0.0.1:$port/" || fail "serve didn't start on HTTP_PORT=3000"
[ "$(http_code "http://127.0.0.1:$port/")" = 401 ] || fail "serve answered without credentials"
[ "$(http_code -u "smoke:$pass_word" "http://127.0.0.1:$port/")" = 200 ] || fail "serve rejected valid credentials"
ok "serve: HTTP_PORT honoured, 401 without and 200 with AUTH_USER/AUTH_PASS"

cmdline=$(docker exec "$prefix-serve" sh -c 'tr "\0" " " </proc/1/cmdline')
for want in '--port 3000' '--flatten=true' '--convert-on-boot=true' '--auth-user smoke'; do
	grep -qF -- "$want" <<<"$cmdline" || fail "pid 1 lacks '$want': $cmdline"
done
uid=$(docker exec "$prefix-serve" awk '/^Uid:/{print $2}' /proc/1/status)
[ "$uid" = 1000 ] || fail "app runs as uid $uid, not 1000"
ok "entrypoint passes FLATTEN/CONVERT_ON_BOOT/AUTH_* as flags and drops to uid 1000"

# ---- custom uid (--user), as on TrueNAS --------------------------------------
start "$prefix-uid" --user 568:568 --tmpfs /home/hfdownloader:uid=568,gid=568 \
	-p 127.0.0.1::8080 "$image" serve
port=$(host_port "$prefix-uid" 8080)
wait_http "http://127.0.0.1:$port/" || fail "serve didn't start as --user 568:568"
[ "$(http_code "http://127.0.0.1:$port/")" = 200 ] || fail "serve as --user 568:568 didn't answer 200"
ok "serve works as --user 568:568"

# ---- SSH / SFTP (SSH_ENABLE=true) --------------------------------------------
docker network create "$prefix-net" >/dev/null
start "$prefix-ssh" --network "$prefix-net" --network-alias hfd \
	-e SSH_ENABLE=true -e SSH_PORT=2222 -e AUTH_USER=smoke -e AUTH_PASS="$pass_word" \
	"$image" serve
for _ in $(seq 1 30); do
	docker logs "$prefix-ssh" 2>&1 | grep -q 'SSH server listening on port 2222' && break
	sleep 1
done
docker logs "$prefix-ssh" 2>&1 | grep -q 'SSH server listening on port 2222' || fail "SSH server didn't start"

out=$(docker run --rm --network "$prefix-net" -e PW="$pass_word" "$client_image" sh -c '
	set -e
	apk add -q --no-cache openssh-client sshpass >/dev/null
	O="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=no -o NumberOfPasswordPrompts=1"
	if sshpass -p wrong ssh $O -p 2222 smoke@hfd true 2>/dev/null; then echo "WRONG-PASSWORD-ACCEPTED"; fi
	sshpass -p "$PW" ssh $O -p 2222 smoke@hfd "echo uid=\$(id -u) HF_HOME=\$HF_HOME"
	echo smoke > /tmp/up.txt
	printf "put /tmp/up.txt .cache/huggingface/models/smoke.txt\nget .cache/huggingface/models/smoke.txt /tmp/down.txt\n" |
		sshpass -p "$PW" sftp $O -P 2222 -b - smoke@hfd >/dev/null
	echo "sftp=$(cat /tmp/down.txt)"
' 2>&1) || fail "ssh/sftp client failed: $out"
grep -q 'WRONG-PASSWORD-ACCEPTED' <<<"$out" && fail "SSH accepted a wrong password"
grep -q 'uid=1000 HF_HOME=/home/hfdownloader/.cache/huggingface' <<<"$out" || fail "ssh session unexpected: $out"
grep -q 'sftp=smoke' <<<"$out" || fail "sftp round trip failed: $out"
owner=$(docker exec "$prefix-ssh" stat -c %u:%g /home/hfdownloader/.cache/huggingface/models/smoke.txt)
[ "$owner" = 1000:1000 ] || fail "file uploaded over SFTP is owned by $owner, not 1000:1000"
ok "SSH_ENABLE: password login as uid 1000, wrong password rejected, SFTP round trip, uploads owned by 1000:1000"

echo "All smoke tests passed for $image"
