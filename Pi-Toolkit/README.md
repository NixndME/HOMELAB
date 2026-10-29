# Pi Toolkit: home Raspberry Pi base setup and health check

The Pi (Raspberry Pi 5, Ubuntu 24.04, static IP on Ethernet) runs several independent app
stacks. Power cuts are frequent, so everything must come back on its own, and one app must
never be able to take another down.

## How the Pi is laid out

One folder and one compose project per app, all with `restart: always`:

| Folder | What | Public access |
|---|---|---|
| `~/poc-bible-app` | POC tracker app + Postgres + backups (own repo) | its own `cloudflared` container |
| `~/cp` | [cp](../cp.nixndme.com), clipboard for text/files/links | its own `cloudflared` container (`cp-tunnel`) |
| `~/adguard` | [AdGuard Home](../AdGuardHome), DNS ad blocker for the LAN | LAN only (host network, port 53) |
| `~/spend-grid` | spend-grid (own repo) | LAN only |

Rules every stack follows:

- **Comes back after a power cut**: `restart: always` on every container, Docker enabled at boot.
  Databases are configured for crash safety (e.g. cp uses SQLite WAL + `synchronous=FULL`).
- **Isolated**: own compose project, own network, memory/CPU limits, and its **own Cloudflare
  Tunnel** if it is public. No shared reverse proxy and no host-wide `cloudflared` service, so
  changing or restarting one app never touches another app's route.
- **Images come from a registry**: apps are built on a dev machine and pushed to Docker Hub; the
  Pi only pulls. cp updates itself through `cp-update.timer` (see the cp README).
- **No site details in git**: IPs, users, passwords and tunnel credentials live in each folder's
  git-ignored `.env` / `tunnel/`.

## Rebuilding the Pi

1. Flash the OS, set the static IP in netplan, enable SSH with your key.
2. Run the base setup (idempotent and non-destructive, safe to re-run on the live Pi):
   ```bash
   sudo ./setup.sh
   ```
   It installs Docker, enables `docker` and `containerd` at boot, adds your user to the `docker`
   group, blocks WiFi when the cable is in (running both gave two default routes and random
   tunnel drops), and turns on the persistent journal. It never removes containers or edits
   the network.
3. Restore each app folder (its `.env`, data and `tunnel/` from backup) and run
   `docker compose up -d` in it. Follow that app's README.
4. Check everything:
   ```bash
   ./status.sh
   ```

## After a power cut, or after any change

```bash
./status.sh
```

Read-only. Shows boot settings, every container's state, health, restart policy and compose
folder, the update timers, and disk/memory. Exits non-zero and prints `!!` lines if a container
is down or unhealthy, or has a restart policy that would not survive a reboot.

## Adding a new public app

Copy the cp pattern ([compose.yml](../cp.nixndme.com/compose.yml)): its own folder, `restart: always`,
memory/CPU limits, and a `cloudflared` container on the app's own network:

```bash
cloudflared tunnel create <app>                 # on a machine logged in to Cloudflare
# put <tunnel-id>.json + config.yml (ingress -> http://<service>:<port>) in ~/<app>/tunnel/
sudo chown -R 65532:65532 ~/<app>/tunnel && sudo chmod -R go-rwx ~/<app>/tunnel
```

Then add a proxied CNAME `<app>` → `<tunnel-id>.cfargotunnel.com` in the zone (Cloudflare API or
dashboard).

## Troubleshooting

- **Everything feels slow**: check `ethtool -S eth0` for `rx_ip_header_checksum_errors`. A high or
  climbing count means a bad cable or port, not software (once: 21k+ errors, 50%+ packet loss).
- **DNS**: AdGuard Home listens on the LAN address port 53; `systemd-resolved` keeps its own stub on
  127.0.0.53, so they do not clash. Test with `dig @<pi-ip> example.com`.
