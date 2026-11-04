# cp: private clipboard for text, files and links

A single-user, self-hosted drop box. Paste text (any length), upload files (up to 100 MB each) or
save a link from any browser; pick it up later from another machine. Everything expires and is
**hard-deleted** on schedule. Data never leaves the host it runs on.

- **Login**: one account (argon2id password, lockout after 5 failures, optional TOTP 2FA).
- **Sessions**: a login ends after 15 minutes without activity (10, 30 or 60 in Settings) and after
  24 hours at most. Background refreshes of an open tab do not count as activity. At most 5 logins
  at once: a 6th ends the least recently used one. Settings lists every login (device, IP, last
  active) with its own Log out button.
- **Dashboard**: every share (active and ended), search, storage-usage bar, paste (Ctrl+V) or drop
  files anywhere to create a share.
- **Share types**: text (syntax highlighting, raw view for `curl`), files (multi-file, zip download,
  inline PDF/image preview), links.
- **Per share**: expiry, public link on/off, QR code, password, burn after reading, max downloads,
  new link (old URL dies), access log, delete.
- **Share page** (`/s/<id>`): shows only that one item. IDs are 22 random base62 characters (~131 bits).
  Unknown, expired and private links all return the same 404.
- **Live transfer** (`/live`): send big files or whole folders (5, 10 GB, more) from one device straight
  to another on the home network. Open Live on both devices, pick files on one, tap Receive on the other.
  See [Live transfer](#live-transfer).
- **Phone app**: install cp from the browser (Android: menu → Install app) and it shows up in the
  share sheet: **Share → cp** on a photo, file, link or text opens New share with it filled in.
  See [Phone app](#phone-app).
- **Terminal** (`cpup`): `cpup file.pdf`, `cat log.txt | cpup -e 1h`, `cpup https://…` with an API
  token that can only create shares. See [Terminal uploads](#terminal-uploads-cpup).
- **Settings**: password, username, 2FA, default expiry, cleanup interval, global public-sharing
  switch, history retention, auto log out time, active logins (end one or all others), API tokens.

## Design notes

| Concern | How |
|---|---|
| Cloudflare 100 MB request limit | All uploads (text too) go in 8 MB ordered chunks, with progress and retries |
| Huge pastes | The page previews the first 5,000 lines; Copy all / Download / Raw always return the full text |
| Encryption at rest | AES-256-GCM, 1 MiB authenticated records bound to the blob id; the key lives in `CP_KEY_FILE` |
| Power cuts | SQLite WAL + `synchronous=FULL`; an orphan sweep at boot removes half-deleted files; `restart: always` |
| Disk safety | `CP_STORAGE_CAP_MB` hard cap; uploads are refused when it would be exceeded |
| Web security | Strict CSP (no inline script), CSRF token plus same-origin check, `Referrer-Policy: same-origin`, `noindex`, user files never rendered as HTML |
| Container | distroless, non-root, read-only root fs, all capabilities dropped, 256 MB / 1 CPU limits, loopback-only port |

## Phone app

cp ships a web app manifest and a small service worker, so Chrome, Edge and Samsung Internet on
Android can install it, and it then appears in the system share sheet (Web Share Target).

- Sharing to cp opens the dashboard with **New share** pre-filled: the title as the name, a page
  address as a link, other text as text, files as files. You still pick the options and tap Create;
  uploads use the normal chunked path, so the 100 MB per file limit applies.
- What was shared waits in that browser (IndexedDB) until the dashboard picks it up, so it survives
  having to log in first. It is taken out once, and dropped if not picked up within an hour.
- The service worker only answers the share sheet's `POST /share-target`. It caches nothing and
  never sees other requests; it is registered only on owner pages, never for share visitors.
- iPhone/iPad: Safari can add cp to the Home screen but has no share sheet support for web apps.

## Terminal uploads (cpup)

`cpup` is a bash + curl script served by cp itself. It uses an **API token** (Settings → API
tokens; needs your password, shown once, stored hashed, at most 10, revocable). A token can only
upload and create shares: it cannot list, open, change or delete them, or reach settings or the
account. Ten wrong tokens from one IP lock it out for 15 minutes; `cp reset-password` revokes all.

```sh
mkdir -p ~/.local/bin && curl -fsSL https://cp.example.com/cpup -o ~/.local/bin/cpup && chmod +x ~/.local/bin/cpup
cpup --setup                       # saves address + token in ~/.config/cpup/config (mode 600)
echo hello | cpup                  # text share
cpup -t "logs" -e 6h -p a.log b.png  # files, name, 6 hour expiry, public link
cpup https://example.com/page      # link share
cpup --help
```

It prints the share link. Files go up in the server's chunk size with retries, the token is passed
to curl through a private file (never on the command line), and `CP_URL` / `CP_TOKEN` override
the config file. Raw API: `Authorization: Bearer cpt_…` on `POST /api/uploads`,
`PUT /api/uploads/{id}/{n}` and `POST /api/shares`; no CSRF header is needed with a token.

## Live transfer

For files too big for a share (videos, disk images, zip archives): nothing is uploaded. cp only
introduces the two browsers; the bytes go **device to device** over WebRTC, encrypted (DTLS), at home
network speed, so they never touch the Pi's disk or go through Cloudflare.

```
laptop A ──handshake (a few KB)──▶ cp (Pi) ◀──handshake── laptop B / phone
laptop A ════════════ file data, directly over the LAN ════════════▶ laptop B / phone
```

- Both devices must have the page open at the same time; the sender's tab must stay open.
- **Where it lands** on the receiving device:
  - Chrome, Edge, Opera (desktop): you pick a folder (or file name) and data is written straight into it.
  - Safari, Firefox, phones: the browser stores it first, then you tap **Save** for each file. A picked
    folder arrives as one `.zip` with its layout kept; loose files (videos, zips) stay separate. This
    needs free space for about twice the size until you tap **Done**.
- Flow control: the sender never gets more than 32 MB ahead of what the receiver has written to disk,
  so memory stays flat for any size. File sizes are checked byte for byte.
- `CP_LIVE_STUN` (default: Cloudflare's public STUN server) only helps each browser learn its own
  address when local discovery is blocked; no file data goes there. Set `CP_LIVE_STUN=none` to use
  local network addresses only.
- If devices cannot connect directly (most often a laptop firewall such as `ufw` or Windows
  Firewall dropping incoming traffic), the optional [relay](#relay) on the Pi carries the data
  instead. Without it: allow incoming UDP from the LAN on that device, use the same Wi-Fi/LAN (not
  a guest network with client isolation), and no VPN.

### Relay

A small TURN relay (coturn, compose profile `turn`) on the Pi, used only when the direct path fails.
Each browser connects **out** to the relay, which firewalls allow, and the relay forwards the data:

```
laptop A ──▶ relay (Pi, LAN) ──▶ laptop B        still end-to-end encrypted (DTLS); nothing stored
```

- Home network only: it listens on the Pi's LAN address (`CP_TURN_LISTEN_IP`) and forwards only to
  `CP_TURN_ALLOWED_PEERS`; anything else (internet, Docker networks, loopback) is refused.
- No accounts: cp hands each Live page a login valid for 24 hours, signed with
  `CP_LIVE_TURN_SECRET` (TURN REST scheme), so only someone logged in to cp can use the relay.
- Limits: 8 relay connections per login, 32 in total, 128 MB RAM, one CPU.
- The browsers pick it automatically; a direct connection is always tried and preferred.

Turn it on (on the Pi, in `.env`):

```sh
CP_LIVE_TURN_SECRET=$(openssl rand -hex 32)     # paste the value; letters and digits only
CP_LIVE_TURN=turn:<pi-lan-ip>:3478?transport=udp,turn:<pi-lan-ip>:3478?transport=tcp
CP_TURN_LISTEN_IP=<pi-lan-ip>
CP_TURN_ALLOWED_PEERS=<lan-first>-<lan-last>   # e.g. 192.168.0.0-192.168.0.255
COMPOSE_PROFILES=tunnel,turn
```

then `docker compose up -d`. The relay refuses to start without a 32+ character secret and a listen
address. To rotate the secret, put a new value in `.env` and run `docker compose up -d`; open Live
pages need a reload. If the Pi itself runs a firewall, allow UDP+TCP 3478 and UDP
49160-49200 from the LAN. The Pi needs a fixed LAN address (DHCP reservation).

## Run it

The host only needs `compose.yml`, `.env` and (for the tunnel) a `tunnel/` folder. The image is
always pulled from Docker Hub (`nixndme/cp`); nothing is built on the host.

```sh
mkdir -p ~/cp/data ~/cp/tunnel && cd ~/cp     # copy compose.yml and .env.example here
cp .env.example .env      # set CP_BASE_URL, CP_ADMIN_USER, CP_ADMIN_PASSWORD, CP_UID/CP_GID
docker compose up -d
```

The admin variables are only read on first boot; afterwards change the password in Settings.

Forgot the password? `docker compose run --rm -e CP_NEW_PASSWORD='…' cp reset-password`
(turns 2FA off and logs out every session).

### Dedicated Cloudflare Tunnel

cp brings its own `cloudflared` container (`tunnel` profile, on by default via `COMPOSE_PROFILES`),
so it never shares or changes another stack's tunnel:

```sh
cloudflared tunnel create cp                      # on any machine logged in to Cloudflare
cp ~/.cloudflared/<tunnel-id>.json tunnel/        # credentials
cp deploy/tunnel-config.example.yml tunnel/config.yml   # fill in the id and hostname
sudo chown -R 65532:65532 tunnel && sudo chmod -R go-rwx tunnel   # cloudflared runs as 65532
```

Then point the hostname at it: a proxied CNAME `cp` → `<tunnel-id>.cfargotunnel.com`.

### Survives power cuts

All containers use `restart: always` and Docker is enabled at boot, so they come back by themselves.
SQLite runs in WAL mode with `synchronous=FULL`, and a sweep at startup removes half-written files.

### Automatic updates

`deploy/cp-update.timer` checks Docker Hub every 10 minutes (and 3 minutes after boot), pulls
`nixndme/cp:latest` and recreates only cp's own containers when the image changed:

```sh
# edit User= and WorkingDirectory= in deploy/cp-update.service first
sudo cp deploy/cp-update.service deploy/cp-update.timer /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now cp-update.timer
```

## Release (amd64 + arm64)

Build and push from a dev machine; hosts pick it up within ~10 minutes:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t nixndme/cp:latest -t nixndme/cp:$(git rev-parse --short HEAD) --push .
```

Roll back by setting `CP_IMAGE=nixndme/cp:<old-sha>` in `.env` and running `docker compose up -d`.

## Tests

```sh
# unit + integration (no local Go needed)
docker run --rm -v "$PWD":/src -w /src golang:1-alpine go test ./...
# headless end-to-end against a running instance (reads credentials from .env)
cd qa && npm install && node e2e.js            # CP_URL=http://localhost:8090 to skip the tunnel
# login sessions (5-login cap, ending one, auto log out); logs in many times, so throwaway instance
# only. IDLE_WAIT=1 adds a real 5-minute idle logout (~6 min).
CP_URL=http://localhost:18091 SESS_USER=tester SESS_PASS=<pick-one> node qa/sessions-e2e.js
# phone app (installable, share sheet incl. files and the logged-out case) + API tokens + the real
# cpup script (20 MB multi-chunk file checked by sha256); creates/revokes tokens, so throwaway only
CP_URL=http://localhost:18092 APP_USER=tester APP_PASS=<pick-one> node qa/app-e2e.js
# streaming ZIP writer used by Live transfer (needs python3 and unzip)
node --test qa/
# Live transfer between real engines (Firefox ≈ Zen, WebKit ≈ Safari, Chromium ≈ Opera), against a
# throwaway local instance; runs in the Playwright image so WebKit works on any distro
docker build -t cp-live-test . && docker run -d --name cp-live-test -p 127.0.0.1:18090:8080 \
  -e CP_BASE_URL=http://localhost:18090 -e CP_ADMIN_USER=tester -e CP_ADMIN_PASSWORD=<pick-one> cp-live-test
docker run --rm --network host --ipc=host -v "$PWD/qa":/qa:ro -w /qa -e LIVE_USER=tester -e LIVE_PASS=<pick-one> \
  mcr.microsoft.com/playwright:v1.64.1-noble sh -c 'apt-get -qq update && apt-get -qq install -y unzip && node live-e2e.js 64'
# Same, forced through the relay (as behind a blocking firewall): start the relay with the turn
# profile on this machine's LAN IP, give the cp container CP_LIVE_TURN + CP_LIVE_TURN_SECRET, and add
# -e LIVE_RELAY_ONLY=1 to the Playwright run.
```
