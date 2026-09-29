#!/bin/sh
# Claude S40 server on THIS machine's Docker (no SSH, no compose): an Unraid
# server (run it in the Unraid terminal) or your own computer at home.
# The image is built here from this checkout, so nothing is pulled from or
# pushed to a registry.
#
#   deploy/local.sh up                    build the image, (re)start the container
#   deploy/local.sh pair <6-digit code> [name]
#   deploy/local.sh devices | revoke <dev-xxxxxxxx> | logs [n]
#   deploy/local.sh claude-login | claude-status | claude-logout   (subscription backend)
#   deploy/local.sh serve-ca [minutes]    serve only the root CA for the phone (default 10)
#
# Files, in S40_DIR (default /mnt/user/appdata/claude-s40-server on Unraid,
# else ~/claude-s40-server):
#   certs/server-chain.pem, certs/server.key   from scripts/pki.sh (copy them here)
#   certs/claude-s40-ca.cer                    only for serve-ca
#   secrets/admin_token                        generated here if missing
#   secrets/anthropic_api_key                  only for CLAUDE_BACKEND=api
#   settings.env                               created on the first "up" (test mode)
# Pairings, chats and the Claude Code login live in the Docker volumes
# claude-s40-data and claude-s40-home, not in S40_DIR.
#
# Ports: S40_PORT (default 8443 on Unraid, whose web UI has 443; else 443)
# -> the phone port; forward TCP 443 on your router to it. The admin API is
# published on 127.0.0.1:9090 only. serve-ca uses S40_CA_PORT (8080 on
# Unraid, else 80); forward TCP 80 to it only while it runs.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
UNRAID=no; [ -f /etc/unraid-version ] && UNRAID=yes
if [ "$UNRAID" = yes ]; then
	DIR=${S40_DIR:-/mnt/user/appdata/claude-s40-server}; PORT=${S40_PORT:-8443}; CA_PORT=${S40_CA_PORT:-8080}
else
	DIR=${S40_DIR:-$HOME/claude-s40-server}; PORT=${S40_PORT:-443}; CA_PORT=${S40_CA_PORT:-80}
fi
NAME=claude-s40-server
# the container runs as uid 65532: on Linux (Unraid too) the key and the
# secrets must be its own; Docker Desktop on a Mac maps file access to your
# user by itself, so nothing is changed there
LINUX=no; [ "$(uname -s)" = Linux ] && LINUX=yes
S=""; [ "$LINUX" = yes ] && [ "$(id -u)" != 0 ] && S=sudo
CMD=${1:?usage: see the header of deploy/local.sh}; shift

token() { $S cat "$DIR/secrets/admin_token"; }
admin() {  # METHOD PATH [BODY]
	curl -sS -X "$1" -H "Authorization: Bearer $(token)" ${3:+-d "$3"} "http://127.0.0.1:9090$2"; echo
}
setting() { sed -n "s/^$1=//p" "$DIR/settings.env" | tail -n 1; }

case "$CMD" in
up)
	for f in server-chain.pem server.key; do
		[ -f "$DIR/certs/$f" ] || { echo "missing $DIR/certs/$f (from scripts/pki.sh)" >&2; exit 1; }
	done
	mkdir -p "$DIR/secrets"
	if ! $S test -s "$DIR/secrets/admin_token"; then
		(umask 077; openssl rand -hex 32 > "$DIR/secrets/admin_token")
	fi
	if [ ! -f "$DIR/settings.env" ]; then
		cat > "$DIR/settings.env" <<-'ENV'
		# Claude S40 server settings (read by deploy/local.sh up; no secrets here)
		# MOCK_ANTHROPIC=1: fake "[Test mode]" replies. 0: real Claude calls.
		MOCK_ANTHROPIC=1
		# claude-code: your Claude subscription (log in: local.sh claude-login;
		# personal use only). api: API key in secrets/anthropic_api_key.
		CLAUDE_BACKEND=claude-code
		CLAUDE_MODEL=claude-opus-5
		CLAUDE_EFFORT=low
		DAILY_REQUEST_LIMIT=100
		DAILY_OUTPUT_TOKEN_LIMIT=100000
		WEB_SEARCH=1
		WEB_SEARCH_MAX_USES=3
		DAILY_SEARCH_LIMIT=30
		DAILY_IMAGE_LIMIT=30
		TRANSCRIBE=off
		ENV
		echo "created $DIR/settings.env (test mode)"
	fi
	BACKEND=$(setting CLAUDE_BACKEND); BACKEND=${BACKEND:-claude-code}
	VERSION=$(sed -n 's/^[[:space:]]*version[[:space:]]*= "\(.*\)"$/\1/p' "$HERE/main.go")
	case "$BACKEND" in
	claude-code) IMAGE=$NAME:$VERSION-claude-code; TARGET="--target claude-code"; MEM=768m ;;
	api)
		IMAGE=$NAME:$VERSION; TARGET=""; MEM=128m
		if [ "$(setting MOCK_ANTHROPIC)" = 0 ] && ! $S test -s "$DIR/secrets/anthropic_api_key"; then
			echo "refusing: MOCK_ANTHROPIC=0 with CLAUDE_BACKEND=api but no $DIR/secrets/anthropic_api_key" >&2; exit 1
		fi ;;
	*) echo "CLAUDE_BACKEND in settings.env must be claude-code or api" >&2; exit 1 ;;
	esac
	echo "== build $IMAGE (tests run in the build; the first build takes several minutes)"
	# shellcheck disable=SC2086
	DOCKER_BUILDKIT=1 docker build $TARGET -t "$IMAGE" "$HERE"
	if [ "$LINUX" = yes ]; then
		$S chown -R 65532:65532 "$DIR/certs/server.key" "$DIR/secrets"
		$S find "$DIR/secrets" -type f -exec chmod 400 {} +
		$S chmod 400 "$DIR/certs/server.key"
		$S chmod 500 "$DIR/secrets"
	fi
	echo "== start $NAME (port $PORT for the phone, admin on 127.0.0.1:9090)"
	docker rm -f "$NAME" >/dev/null 2>&1 || true
	# the same limits as compose.yaml (+ compose.claude-code.yaml)
	docker run -d --name "$NAME" --restart unless-stopped \
		-p "$PORT:8443" -p 127.0.0.1:9090:9090 \
		-v "$DIR/certs:/certs:ro" -v "$DIR/secrets:/run/secrets:ro" \
		-v claude-s40-data:/data -v claude-s40-home:/home/nonroot \
		--env-file "$DIR/settings.env" -e ADMIN_LISTEN=0.0.0.0:9090 -e HOME=/home/nonroot \
		--read-only --tmpfs /tmp:size=64m,mode=1777 --cap-drop ALL --security-opt no-new-privileges:true \
		--memory "$MEM" --pids-limit 128 --log-driver json-file --log-opt max-size=10m --log-opt max-file=5 \
		"$IMAGE" >/dev/null
	sleep 2
	docker logs --tail 3 "$NAME"
	if [ "$BACKEND" = claude-code ] && ! docker exec "$NAME" /usr/local/bin/claude auth status >/dev/null 2>&1; then
		echo "Claude Code is not logged in yet: deploy/local.sh claude-login"
	fi ;;
pair)
	CODE=$(printf '%s' "${1:?code}" | tr -cd '0-9'); PNAME=$(printf '%s' "${2:-phone}" | tr -cd 'A-Za-z0-9 ._-')
	[ ${#CODE} -eq 6 ] || { echo "code must be 6 digits" >&2; exit 2; }
	admin POST /admin/pair/approve "{\"code\":\"$CODE\",\"name\":\"$PNAME\"}" ;;
devices) admin GET /admin/devices ;;
revoke)
	ID=$(printf '%s' "${1:?device id}" | tr -cd 'a-z0-9-')
	admin POST /admin/devices/revoke "{\"device_id\":\"$ID\"}" ;;
logs) exec docker logs --tail "${1:-30}" "$NAME" ;;
claude-login) exec docker exec -it "$NAME" /usr/local/bin/claude auth login --claudeai ;;
claude-status) exec docker exec "$NAME" /usr/local/bin/claude auth status --text ;;
claude-logout) exec docker exec "$NAME" /usr/local/bin/claude auth logout ;;
serve-ca)
	# the certificate is public; what protects you is comparing the
	# fingerprint the phone shows with "scripts/pki.sh show" before saving it
	CA="$DIR/certs/claude-s40-ca.cer"
	[ -f "$CA" ] || { echo "missing $CA (copy it from your pki directory)" >&2; exit 1; }
	WWW=$(mktemp -d)
	cleanup() { docker rm -f s40-ca-download >/dev/null 2>&1 || true; rm -rf "$WWW"; }
	trap cleanup EXIT
	trap 'exit 130' INT TERM
	mkdir "$WWW/www" && cp "$CA" "$WWW/www/ca.cer" && chmod 755 "$WWW" "$WWW/www" && chmod 644 "$WWW/www/ca.cer"
	printf '.cer:application/x-x509-ca-cert\n' > "$WWW/httpd.conf" && chmod 644 "$WWW/httpd.conf"
	docker rm -f s40-ca-download >/dev/null 2>&1 || true
	docker run -d --name s40-ca-download --read-only --cap-drop ALL --security-opt no-new-privileges:true \
		--user 65534:65534 --memory 16m -p "$CA_PORT:8080" -v "$WWW/www:/www:ro" -v "$WWW/httpd.conf:/etc/httpd.conf:ro" \
		busybox:stable httpd -f -p 8080 -h /www -c /etc/httpd.conf >/dev/null
	echo "serving http://<your address>/ca.cer on port $CA_PORT for ${1:-10} minutes (Ctrl+C stops)"
	sleep $(( ${1:-10} * 60 )) ;;
*) echo "unknown command $CMD (see the header of deploy/local.sh)" >&2; exit 2 ;;
esac
