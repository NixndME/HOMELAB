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
```
