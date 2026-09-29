#!/bin/sh
# Deploy claude-s40-server to a Docker host over SSH.
#
#   deploy/push.sh SSH_TARGET            plan only (default)
#   deploy/push.sh SSH_TARGET --execute  build here (linux/amd64, tests run), send image, start
#
# Env (S40_ prefix, so unrelated CLAUDE_* shell variables are never picked
# up): PKI_DIR (default ~/.config/claude-s40/pki), S40_MOCK (default 1),
# S40_MODEL, S40_EFFORT, S40_FALLBACKS, S40_REQ_LIMIT, S40_TOK_LIMIT,
# S40_ENVIRONMENT, S40_SEARCH (1/0), S40_SEARCH_MAX_USES (per message),
# S40_SEARCH_LIMIT (per device per day), S40_SEARCH_COUNTRY/_CITY/_TIMEZONE
# (optional approximate location for local search results),
# S40_TRANSCRIBE (voice messages: off (default), mock, openai),
# S40_TRANSCRIBE_MODEL, S40_TRANSCRIBE_LIMIT (per device per day),
# S40_IMAGE_LIMIT (photo uploads per device per day),
# S40_BACKEND (api (default): Claude API key; claude-code: your Claude
# subscription through the Claude Code CLI, see compose.claude-code.yaml).
# Copies only: image, compose.yaml (+ compose.claude-code.yaml), .env (no
# secrets), server-chain.pem, server.key.
# On the server: secrets/admin_token is generated there if missing (never
# leaves the server); secrets/anthropic_api_key must be put there with
# deploy/set-key.sh (and secrets/openai_api_key with "set-key.sh TARGET
# openai" for TRANSCRIBE=openai). With S40_BACKEND=claude-code there is no
# API key: log in once with "deploy/admin.sh TARGET claude-login" (the login
# stays in the container's claudecode volume). The CA key never leaves the Mac.
set -eu
HERE=$(cd "$(dirname "$0")/.." && pwd)
TARGET=${1:?usage: deploy/push.sh SSH_TARGET [--execute]}
EXEC=no; [ "${2:-}" = "--execute" ] && EXEC=yes
PKI=${PKI_DIR:-$HOME/.config/claude-s40/pki}
REMOTE=claude-s40-server
BACKEND=${S40_BACKEND:-api}
case "$BACKEND" in
api) IMAGE=claude-s40-server:0.7.0 TARGET_ARG="" ;;
claude-code) IMAGE=claude-s40-server:0.7.0-claude-code TARGET_ARG="--target claude-code" ;;
*) echo "S40_BACKEND must be api or claude-code" >&2; exit 1 ;;
esac
MOCK=${S40_MOCK:-1}
STT=${S40_TRANSCRIBE:-off}
case "$STT" in off|mock|openai) ;; *) echo "S40_TRANSCRIBE must be off, mock or openai" >&2; exit 1 ;; esac

for f in server-chain.pem server.key; do
	[ -f "$PKI/$f" ] || { echo "missing $PKI/$f" >&2; exit 1; }
done
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
grep -v '^    build: \.$' "$HERE/compose.yaml" > "$STAGE/compose.yaml"
if [ "$BACKEND" = claude-code ]; then
	grep -v -e '^    build:$' -e '^      context: \.$' -e '^      target: claude-code$' \
		"$HERE/compose.claude-code.yaml" > "$STAGE/compose.claude-code.yaml"
fi
mkdir -m 755 "$STAGE/certs"
cp "$PKI/server-chain.pem" "$STAGE/certs/" && chmod 644 "$STAGE/certs/server-chain.pem"
cp "$PKI/server.key" "$STAGE/certs/" && chmod 600 "$STAGE/certs/server.key"
cat > "$STAGE/.env" <<ENV
ENVIRONMENT=${S40_ENVIRONMENT:-production}
CLAUDE_MODEL=${S40_MODEL:-claude-opus-5}
CLAUDE_EFFORT=${S40_EFFORT:-low}
CLAUDE_FALLBACKS=${S40_FALLBACKS:-default}
MOCK_ANTHROPIC=$MOCK
DAILY_REQUEST_LIMIT=${S40_REQ_LIMIT:-100}
DAILY_OUTPUT_TOKEN_LIMIT=${S40_TOK_LIMIT:-100000}
WEB_SEARCH=${S40_SEARCH:-1}
WEB_SEARCH_MAX_USES=${S40_SEARCH_MAX_USES:-3}
DAILY_SEARCH_LIMIT=${S40_SEARCH_LIMIT:-30}
SEARCH_COUNTRY=${S40_SEARCH_COUNTRY:-}
SEARCH_CITY=${S40_SEARCH_CITY:-}
SEARCH_TIMEZONE=${S40_SEARCH_TIMEZONE:-}
TRANSCRIBE=$STT
TRANSCRIBE_MODEL=${S40_TRANSCRIBE_MODEL:-gpt-4o-mini-transcribe}
DAILY_TRANSCRIBE_LIMIT=${S40_TRANSCRIBE_LIMIT:-30}
DAILY_IMAGE_LIMIT=${S40_IMAGE_LIMIT:-30}
ENV
[ "$BACKEND" = claude-code ] && echo "COMPOSE_FILE=compose.yaml:compose.claude-code.yaml" >> "$STAGE/.env"

echo "== plan"
echo "target  : $TARGET:~/$REMOTE"
echo "image   : $IMAGE (built here for linux/amd64; go vet + tests run in the build)"
echo "settings:"; sed 's/^/  /' "$STAGE/.env"
echo "cert    : $(${OPENSSL:-openssl} x509 -in "$PKI/server.pem" -noout -subject -enddate 2>/dev/null | tr '\n' ' ')"
echo "ports   : 443 -> 8443 (phone TLS), 127.0.0.1:9090 (admin, server-local only)"
echo "data    : Docker volume claude-s40-server_s40data (SQLite)"
echo "backend : $BACKEND"
if [ "$BACKEND" = claude-code ]; then
	echo "cli     : Claude Code CLI in the image; its login in volume claude-s40-server_claudecode"
	[ "$MOCK" = 0 ] && echo "LIVE    : MOCK_ANTHROPIC=0 -> real Claude calls on your Claude subscription (its usage limits);"
	[ "$MOCK" = 0 ] && echo "          needs a login: deploy/admin.sh $TARGET claude-login. Pair only your own phones."
else
	[ "$MOCK" = 0 ] && echo "LIVE    : MOCK_ANTHROPIC=0 -> real, paid Claude calls; needs secrets/anthropic_api_key"
	[ "$MOCK" = 0 ] && [ "${S40_SEARCH:-1}" = 1 ] && echo "SEARCH  : web search on -> billed per search, results count as input tokens"
fi
[ "$STT" = openai ] && echo "VOICE   : TRANSCRIBE=openai -> paid speech-to-text calls; needs secrets/openai_api_key"
if [ "$EXEC" != yes ]; then
	echo; echo "PLAN ONLY. Run again with --execute after approval."
	exit 0
fi

echo "== build"
docker buildx build --platform linux/amd64 $TARGET_ARG -t "$IMAGE" --load "$HERE" >/dev/null
echo "== send image"
docker save "$IMAGE" | gzip | ssh "$TARGET" 'S=""; [ "$(id -u)" = 0 ] || S=sudo; gunzip | $S docker load'
echo "== copy"
ssh "$TARGET" "mkdir -p ~/$REMOTE/secrets"
scp -q -r "$STAGE"/. "$TARGET:$REMOTE/"
echo "== start"
ssh "$TARGET" "MOCK=$MOCK STT=$STT BACKEND=$BACKEND sh -s" <<'REMOTE_SH'
set -eu
S=""; [ "$(id -u)" = 0 ] || S=sudo
cd ~/claude-s40-server
[ -s secrets/admin_token ] || $S sh -c 'umask 077; openssl rand -hex 32 > secrets/admin_token'
if [ "$MOCK" = 0 ] && [ "$BACKEND" = api ] && ! $S test -s secrets/anthropic_api_key; then
	echo "refusing: MOCK_ANTHROPIC=0 but secrets/anthropic_api_key is missing (use deploy/set-key.sh)" >&2
	exit 1
fi
if [ "$STT" = openai ] && ! $S test -s secrets/openai_api_key; then
	echo "refusing: TRANSCRIBE=openai but secrets/openai_api_key is missing (use deploy/set-key.sh TARGET openai)" >&2
	exit 1
fi
$S chown 65532:65532 certs/server.key secrets secrets/*
$S chmod 400 certs/server.key secrets/*
$S chmod 500 secrets
$S docker compose up -d --force-recreate
sleep 2
$S docker compose ps
$S docker compose logs --tail 3
if [ "$BACKEND" = claude-code ] && ! $S docker compose exec -T server /usr/local/bin/claude auth status >/dev/null 2>&1; then
	echo "Claude Code is not logged in yet: deploy/admin.sh SSH_TARGET claude-login"
fi
REMOTE_SH
