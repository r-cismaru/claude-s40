# Setup guide

From zero to "Claude answers on my Nokia". Allow an hour the first time.
Commands run on your computer (macOS or Linux) unless stated otherwise.

**What you need**

- A Nokia Series 40 phone with Java (CLDC 1.1 / MIDP 2.0). Verified: Nokia
  6300 RM-217, V06.60. A SIM with mobile data.
- A Docker host with a **public IPv4 address** and TCP 443 reachable from
  the internet: a small VPS is enough (512 MB RAM). Cloud platforms that
  terminate HTTPS for you (Cloudflare, App Platform, Vercel, ...) do **not**
  work, the phone cannot complete their TLS handshake.
- A Claude API key (https://console.anthropic.com). Set a spending limit.
  Or, for a server only you use: your Claude Pro or Max subscription
  instead of a key (see "Your Claude subscription" in step 8).
- On your computer: Go 1.26+, JDK 11+, Python 3 with Pillow (`pip install
  pillow`), OpenSSL 3 or LibreSSL, Docker with buildx, ssh.
- A way to install a Java app on the phone: Gammu over USB, Bluetooth file
  transfer, Nokia PC Suite, or the phone browser (step 5).

Throughout: `SERVER=root@<server-ip>` and `IP=<server-ip>`.

---

## 1. Server host

Any Linux host with Docker Engine + the compose plugin. Firewall: allow
**443/tcp** from anywhere and **22/tcp** only from your own IP.

Optional DigitalOcean helper (Ubuntu 24.04 + Docker via cloud-init, own
cloud firewall, prints a plan and creates nothing unless `--execute`):

```
cd server
DO_CONTEXT=default SSH_FROM=<your-ip>/32 deploy/do-create.sh            # plan
DO_CONTEXT=default SSH_FROM=<your-ip>/32 deploy/do-create.sh --execute  # creates droplet + firewall (billed)
```

Wait until `ssh $SERVER docker --version` works (a few minutes).

**At home instead of a VPS** (e.g. your own Mac with Docker Desktop): the
phone comes in over mobile data, so your computer must be reachable from
the internet.
- Your router needs a public IPv4 address (no CGNAT). If the WAN address
  in the router differs from what https://ifconfig.me shows, or is in
  100.64-100.127.x.x, 10.x or 192.168.x, port forwarding cannot work: ask
  your provider for a public IP or use a VPS.
- Use a dynamic DNS name (from the router or e.g. DuckDNS), since the home
  IP changes. Issue the certificate for that name in step 2 and use it as
  `GATEWAY_URL` in step 4.
- Forward TCP 443 on the router to the computer, reserve its LAN address,
  and keep the computer awake. HTTPS tunnels (Cloudflare Tunnel, ngrok)
  do not work, because the phone cannot complete their TLS handshake.
- There is no SSH step: instead of `push.sh`, put `certs/server-chain.pem`,
  `certs/server.key`, `secrets/admin_token` (`openssl rand -hex 32`) and
  `.env` (from `.env.example`) in `server/`, then run `docker compose up -d --build`
  there. It builds for the computer's own CPU; the subscription image
  also runs on Apple silicon (arm64). On Linux, make the key and token
  readable by the container's user (uid 65532). The admin commands become
  `curl` calls to `http://127.0.0.1:9090` with the token (see `deploy/admin.sh`), and
  `docker compose exec server /usr/local/bin/claude auth login` for the
  subscription login.

## 2. Your private certificate authority

The phone will trust **only** this root. Keep `~/.config/claude-s40/pki`
private and backed up; it never goes to the server except the server
certificate and key.

```
server/scripts/pki.sh ca     ~/.config/claude-s40/pki
server/scripts/pki.sh server ~/.config/claude-s40/pki $IP        # or a DNS name you own
server/scripts/pki.sh show   ~/.config/claude-s40/pki            # note the root's SHA-1 / MD5 fingerprints
```

Defaults: RSA-2048, SHA-1 signatures (the combination verified on a Nokia
6300; SHA-256 is untested there), root valid 10 years, server cert 2 years.
If you use a DNS name, point it straight at the server (no proxy/CDN).

## 3. Deploy the server (test mode first)

```
cd server
make test                               # optional: go vet + tests
deploy/push.sh $SERVER                  # shows the plan
deploy/push.sh $SERVER --execute        # build here, send image, start on :443
```

This starts with `MOCK_ANTHROPIC=1`: replies are fake ("[Test mode]") and
cost nothing. On the server you get `~/claude-s40-server/` with
`compose.yaml`, `.env`, `certs/` and `secrets/admin_token` (generated there,
never copied off).

Check it the way the phone will talk to it (TLS 1.0, no SNI, your CA only):

```
deploy/smoke.sh $IP 443 ~/.config/claude-s40/pki/ca.pem "$PWD/deploy/admin.sh $SERVER pair \$1 smoke"
deploy/admin.sh $SERVER devices          # then: deploy/admin.sh $SERVER revoke <smoke device id>
```

## 4. Build the phone app

```
echo "GATEWAY_URL=https://$IP" > app/app.local.properties
make -C app            # build + 44 checks + reproducible rebuild
ls app/dist            # ClaudeS40.jad, ClaudeS40.jar, SHA256SUMS
```

(Without `app.local.properties` the address can be typed in the app's
Settings instead.)

## 5. Install the app on the phone

Pick one:

- **Gammu over USB** (phone in "Nokia mode"/PC Suite mode):
  `app/tools/install-gammu.sh` (dry run), then
  `app/tools/install-gammu.sh --execute --i-understand-this-writes-to-the-phone`.
  It never overwrites; delete an older Claude S40 in the phone menu first.
- **Bluetooth**: send `ClaudeS40.jar` to the phone, open it from the inbox.
- **Nokia PC Suite** (Windows): Install applications.
- **Browser (OTA)**: serve `ClaudeS40.jad` (`text/vnd.sun.j2me.app-descriptor`)
  and `.jar` (`application/java-archive`) from any web server the phone can
  open, then open the JAD URL on the phone.

On the Nokia 6300 the app appears under Menu → Applications → Collection.
The first network access asks for permission; allow it.

## 6. Put your root CA on the phone

The phone must trust your root before the connection test can pass.

1. Temporarily allow **80/tcp** on the server firewall.
2. `server/deploy/serve-ca.sh $SERVER 10` (serves only `ca.cer` for 10 minutes
   and prints the fingerprints to compare).
3. On the phone's browser open `http://<server-ip>/ca.cer`. Before saving,
   compare the fingerprint the phone shows with the printed one. **Do not
   save it if it differs.** Save it as an authority certificate and allow
   it for applications / connections if asked.
4. Close 80/tcp again.

Also make sure the phone's mobile data works (open any plain `http://` page
in its browser). On older phones select the operator's *internet* access
point, not an old WAP profile.

## 7. Connection test and pairing

On the phone, in Claude S40. A fresh install opens a setup wizard
(language, server address, connection test, pairing: "Setup 1/4" to "4/4")
that walks through exactly these steps; it can be skipped and reopened from
Settings → Options → Setup wizard.

1. **Connection test → Start**: it checks `/health` and a UTF-8 round trip
   (`/echo`) and shows the TLS version, cipher and certificate. Chat stays
   locked until it passes. If the phone asks to accept an untrusted
   certificate, say **No**: something is wrong with step 6.
2. **Settings → Options → Pair this phone**: a 6-digit code appears.
3. On your computer: `server/deploy/admin.sh $SERVER pair <code> "My Nokia"`.
   The phone fetches its access token by itself within a few seconds.
4. **Chat**: in test mode you get "[Test mode]" replies. That proves the
   whole path works.

## 8. Go live

```
server/deploy/set-key.sh $SERVER                 # in YOUR terminal; hidden input
S40_MOCK=0 server/deploy/push.sh $SERVER --execute
```

Settings you can pass to `push.sh`: `S40_MODEL` (default `claude-opus-5`),
`S40_EFFORT` (`low`), `S40_FALLBACKS` (`default` = server-side refusal
fallback; `off` for models without it), `S40_REQ_LIMIT` (100/day),
`S40_TOK_LIMIT` (100000 output tokens/day), `S40_SEARCH` (`1` = Claude may
search the web, `0` = never), `S40_SEARCH_LIMIT` (30 searches/day per
phone), `S40_SEARCH_MAX_USES` (3 per message), and optionally
`S40_SEARCH_COUNTRY` (e.g. `TR`), `S40_SEARCH_CITY`, `S40_SEARCH_TIMEZONE`
(e.g. `Europe/Istanbul`) for local results. Web searches are billed per
search on top of tokens. Pairings and chats survive redeploys (Docker
volume `s40data`).

### Your Claude subscription instead of an API key (optional)

With Claude Pro or Max you can skip the API key. The server then answers
each message through Anthropic's Claude Code CLI, which ships unmodified
(pinned version and checksum) in a larger image. The CLI keeps its own
login, which you make once in Anthropic's own sign-in flow; the server never
reads it. Run these in YOUR terminal:

```
S40_MOCK=0 S40_BACKEND=claude-code server/deploy/push.sh $SERVER --execute
server/deploy/admin.sh $SERVER claude-login      # open the link, sign in to claude.ai, paste the code back
server/deploy/admin.sh $SERVER claude-status     # shows the signed-in account
```

- **Personal use only.** Anthropic allows subscription sign-in for your own
  use of Claude Code. It does not allow third-party apps to offer claude.ai
  login, or requests from other people routed through your plan
  ([Claude Code legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)).
  Pair only your own phones. If other people use your server, give it an
  API key instead.
- Messages count against your plan's usage limits, shared with claude.ai
  and Claude Code. While the limit is reached the phone shows "Claude is
  busy". The server's daily limits (`S40_REQ_LIMIT`, ...) still apply.
- Differences from the API key: web search uses Claude Code's WebSearch
  tool, so there is no "Web: <sources>" line and `S40_SEARCH_COUNTRY/_CITY/_TIMEZONE`
  and `S40_FALLBACKS` are ignored. The CLI starts for every message, so a
  reply takes a few seconds longer (timeout 120 s). The CLI itself asks once
  more after a refusal and continues a reply that reaches its output limit.
  Those extra calls use your plan's limits, and cost money only if you
  turned on paid extra usage for your plan.
- The host needs about 1 GB RAM: the container may use up to 768 MB,
  because each CLI run needs about 225 MB and at most 2 run at once.
- The login is stored in the Docker volume `claude-s40-server_claudecode`.
  `admin.sh $SERVER claude-logout` signs out. When the login expires, the
  phone shows "No reply (config_error)": run `claude-login` again.
- Back to an API key: `set-key.sh`, then `S40_MOCK=0 push.sh $SERVER --execute`
  (`S40_BACKEND` defaults to `api`).

**Voice messages** (optional, off by default): the server turns a short
recording into text with OpenAI's speech-to-text (paid per use, well
under a cent per 30 s clip with the default model; check OpenAI's prices).

```
server/deploy/set-key.sh $SERVER openai          # OpenAI API key, hidden input
S40_MOCK=0 S40_TRANSCRIBE=openai server/deploy/push.sh $SERVER --execute
```

`S40_TRANSCRIBE=mock` gives "[Test mode]" texts without a key.
`S40_TRANSCRIBE_LIMIT` (30/day per phone), `S40_TRANSCRIBE_MODEL`
(`gpt-4o-mini-transcribe`). On the phone: **Dictate** in the chat's
options or in the editor, speak (up to 30 s), **Done**; the text opens in
the editor, check it and press **Send**. Nothing reaches Claude before
that. The phone asks for microphone access. About shows whether the phone
can record and which formats it reports.

**Photos** (always on, no extra key): in the chat's options or in the
editor, **Add a photo** → take one with the camera or choose one from the
phone (up to 1 MB); it is uploaded, then the editor opens with "What is in
this photo?" to change and **Send**. Follow-up questions in the same chat
still show Claude the photo. `S40_IMAGE_LIMIT` (30 uploads/day per phone).
Each photo adds about 1000 input tokens to every message in its chat
(the newest 3 photos are sent). About shows whether the phone lets apps
use the camera.

Send "Translate to English: Günaydın" from the phone, then "Make it
shorter" in the same chat. Then try Quick prompts → Weather: a reply
marked "searched the web" ends with "Web: <sources>".

On the phone: **Chats** lists earlier conversations (open one to continue
it); in a chat, **0** loads the rest of a long reply (free, Claude is not
asked again), **2/8** page, **1/3** jump between messages, **\*/#** top and
end, **5** write, **7** reading mode (one reply page by page, full width),
**9** text size. **1/3** also select a message; the centre key then offers
shorten, translate, ask about it or open it in the editor (these only fill
the editor, nothing is sent until you press Send). After an error the
centre key retries the same request. Options → Shortcuts lists every key. Settings → Claude: web search on/off, keep the last chat
on the phone for offline reading.

## 9. Day to day

```
server/deploy/admin.sh $SERVER devices           # paired phones
server/deploy/admin.sh $SERVER revoke dev-xxxx   # revoke one
server/deploy/admin.sh $SERVER logs 50           # method/path/status/TLS only, no content
server/deploy/push.sh $SERVER --execute          # update after a code change
```

Updating the app: delete Claude S40 in the phone menu and install the new
build. Since 0.7.2 the app keeps its setup (server address, access code,
language, notes for Claude) in `ClaudeS40/claude-s40-setup.dat` on the
memory card (or in the phone's image folder), which survives deleting the
app: the new build restores it at its first start (the phone asks for file
access) and needs no new pairing. Without that file (older builds, no file
access), run the connection test and pair again, then revoke the old
device. Settings > Options > "Reset setup" deletes the file and opens the
setup wizard. The file holds the access code: if the memory card leaves
your hands, revoke the device.

## Troubleshooting

| Symptom on the phone | Likely cause |
|---|---|
| "Could not connect ... TLS" and nothing in `admin.sh logs` | mobile data / access point; or a CDN/proxy in front of the server |
| "signature not verified" | root CA not saved on the phone (step 6), or a SHA-256 certificate on a phone that only verifies SHA-1 |
| "host name mismatch" | server certificate issued for a different name/IP than the app uses |
| HTTP code but "not a Claude S40 server reply" | operator proxy or wrong address |
| "No credits" | the Claude API account has no credit balance |
| "No reply (config_error)" with `S40_BACKEND=claude-code` | Claude Code is not logged in or the login expired: `admin.sh $SERVER claude-login` |
| "Claude is busy" for hours with `S40_BACKEND=claude-code` | your Claude plan's usage limit is reached; it resets on its own |
| "Daily limit reached" | raise `S40_REQ_LIMIT` / `S40_TOK_LIMIT` |
| Claude no longer searches the web | the phone's daily search budget is used up (`S40_SEARCH_LIMIT`) or web search is off in Settings / `S40_SEARCH=0` |
| "Access code invalid or revoked" | pair again |
| No **Dictate** command | the phone does not let apps record (About → Voice recording) |
| "Voice messages are not turned on on the server" | deploy with `S40_TRANSCRIBE=openai` (or `mock`) |
| No **Add a photo** command | the phone lets apps use neither the camera nor files (About → Camera) |
| "The photo is too large" from the gallery | over 1 MB: take it with the app's camera (640x480) |
| "The server could not read this recording" | the phone's audio format; `admin.sh logs` shows the format, size and length |
