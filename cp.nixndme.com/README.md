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

```sh
cp .env.example .env      # set CP_BASE_URL, CP_ADMIN_USER, CP_ADMIN_PASSWORD, CP_UID/CP_GID
mkdir -p data
docker compose up -d --build
```

Point a Cloudflare Tunnel ingress rule at `http://localhost:${CP_PORT}`. The admin variables are
only read on first boot; afterwards change the password in Settings.

Forgot the password? `docker compose run --rm -e CP_NEW_PASSWORD='…' cp reset-password`
(turns 2FA off and logs out every session).

## Multi-arch image (amd64 + arm64 for a Raspberry Pi)

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t <user>/cp:<tag> --push .
```

## Tests

```sh
# unit + integration (no local Go needed)
docker run --rm -v "$PWD":/src -w /src golang:1-alpine go test ./...
# headless end-to-end against a running instance (reads credentials from .env)
cd qa && npm install && node e2e.js            # CP_URL=http://localhost:8090 to skip the tunnel
```
