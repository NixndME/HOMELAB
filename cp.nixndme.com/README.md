# cp: private clipboard for text, files and links

A single-user, self-hosted drop box. Paste text (any length), upload files (up to 100 MB each) or
save a link from any browser; pick it up later from another machine. Everything expires and is
**hard-deleted** on schedule. Data never leaves the host it runs on.

- **Login**: one account (argon2id password, lockout after 5 failures, optional TOTP 2FA).
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
- **Settings**: password, username, 2FA, default expiry, cleanup interval, global public-sharing
  switch, history retention, log out other sessions.

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
- If devices cannot connect: same Wi-Fi/LAN (not a guest network with client isolation), no VPN.

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

Both containers use `restart: always` and Docker is enabled at boot, so they come back by themselves.
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
# streaming ZIP writer used by Live transfer (needs python3 and unzip)
node --test qa/
# Live transfer between real engines (Firefox ≈ Zen, WebKit ≈ Safari, Chromium ≈ Opera), against a
# throwaway local instance; runs in the Playwright image so WebKit works on any distro
docker build -t cp-live-test . && docker run -d --name cp-live-test -p 127.0.0.1:18090:8080 \
  -e CP_BASE_URL=http://localhost:18090 -e CP_ADMIN_USER=tester -e CP_ADMIN_PASSWORD=<pick-one> cp-live-test
docker run --rm --network host --ipc=host -v "$PWD/qa":/qa:ro -w /qa -e LIVE_USER=tester -e LIVE_PASS=<pick-one> \
  mcr.microsoft.com/playwright:v1.64.1-noble sh -c 'apt-get -qq update && apt-get -qq install -y unzip && node live-e2e.js 64'
```
