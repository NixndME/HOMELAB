# AdGuard Home: ad blocking for the whole home network

Runs on the Raspberry Pi (Ubuntu 24.04 arm64) as its own Docker Compose project in `~/adguard`,
fully separate from anything else on the Pi (a production app + Cloudflare tunnel run there too,
and are never touched).

Site-specific values (IPs, usernames, device owner names) live only in git-ignored files:
`.env` (copy from `.env.example`) and `clients.json` (copy from `clients.example.json`).
The admin password lives in the file named by `AGH_PASSWORD_FILE` (mode 600); keep a copy in Vaultwarden.

| | |
|---|---|
| DNS | `<PI_IP>:53` UDP/TCP, bound to the LAN IP only (systemd-resolved keeps `127.0.0.53`) |
| Dashboard | `http://<PI_IP>:3000`. LAN only, **not** published through the tunnel |
| Upstreams | DNS-over-HTTPS Quad9 + Cloudflare (parallel), plain `9.9.9.9`/`1.1.1.1` fallback, DNSSEC, optimistic cache |
| Blocklists | AdGuard DNS filter, HaGeZi Multi NORMAL, HaGeZi Threat Intelligence mini (~580k rules, refreshed every 24 h) + Safe Browsing |
| Logs/stats | 7 days (keeps SD-card writes low) |
| Limits | 256 MB RAM, 0.5 CPU (uses ~100 MB) |

## Deploy / re-apply

```bash
cp AdGuardHome/.env.example AdGuardHome/.env   # once, fill in
AdGuardHome/setup.sh
```

Idempotent: copies `docker-compose.yml` to the Pi, `docker compose up -d` (only the `adguard`
project), finishes the first-run wizard via the API if needed, then applies upstreams, blocklists,
Safe Browsing and log retention. "url already exists" (HTTP 400) on re-runs is expected.

Gotchas hit while building it:
- The wizard rejects `<PI_IP>:3000` for the dashboard ("address already in use": the wizard itself
  holds `:3000`), so the dashboard binds `0.0.0.0:3000`. DNS still binds to the LAN IP only.
- `POST /control/safebrowsing/enable` returns 415 if sent with a JSON content-type and no body.
- AdGuard restarts itself once right after the wizard; that's expected.
- No API to change the admin password: stop the container, replace the bcrypt hash (`$2a$`) under
  `users:` in `conf/AdGuardHome.yaml`, start it.

## Power cuts

Verified without rebooting the Pi (production is live on it):
- `docker`/`containerd` are enabled at boot and ordered after `network-online.target`
  (`systemd-networkd-wait-online` is enabled), so the LAN IP exists before AdGuard binds to it.
- `restart: always`. A hard `kill -9` of the AdGuard process was back serving DNS in ~6 s.
  `docker kill`/`docker stop` count as manual stops and are *not* auto-restarted.
- Config (`conf/`) and data (`work/`) are bind mounts and survive restarts.
- The Pi's own DNS does **not** go through AdGuard, so the tunnel and NTP never depend on it.
  No RTC, but timesyncd restores the last-saved clock at boot, so DoH TLS validates; plain
  fallbacks cover the gap otherwise.
- A small DC UPS for router + Pi is still the best protection for the SD card.

## Point the network at it (Tenda router)

System Settings → LAN Settings → DNS Settings on: primary `<PI_IP>`, secondary `1.1.1.1` (keeps the
internet working if the Pi is down; some devices occasionally use the secondary and skip blocking).
Keep the Pi's static IP outside the DHCP range.

**Saving that page restarts the router's LAN side**: the tunnel on the Pi dropped for ~1 min and
re-registered all 4 connections by itself. Do it when nobody depends on the production app.

Devices pick it up on their next DHCP renew (the lease can be days, so toggle Wi-Fi to speed it up).

Devices with their own encrypted DNS ignore the router and bypass AdGuard: Android "Private DNS" set
to a hostname, iCloud Private Relay, VPNs, and Omarchy workstations (resolved pinned to Cloudflare DoT).
For an Android TV that had Private DNS set: `adb shell settings put global private_dns_mode off`
(not "opportunistic": that does encrypted DNS to the `1.1.1.1` secondary and skips the Pi).

## Named devices ("who uses what")

`clients.json` maps each device's IP to a name and tags; `clients.sh` creates or updates them in
AdGuard (idempotent). Pin the IPs on the router: System Settings → DHCP Reservation (the link icon
binds the current lease without restarting the router; **+New** for a device that isn't listed).

```bash
AdGuardHome/clients.sh
```

Viewing it:
- **Android:** [AdGuard Home Manager](https://github.com/jgeek00/adguard-home-manager) (Apache-2.0,
  Google Play or GitHub APK). Server `<PI_IP>`, port `3000`, HTTP.
- **iOS:** no open-source client exists (the App Store app with the same name is a different,
  closed-source developer). Use the built-in dashboard in Safari → Share → Add to Home Screen.

DNS only sees domain names: not data volume (MB/GB) and not page contents.

## Day to day

- Something broken? Dashboard → Query Log → find the blocked domain → **Unblock**.
- Pause blocking: Dashboard → Disable protection (30 s / 1 min / 1 h…).
- Logs: `ssh "$PI_SSH" 'docker logs --tail 50 adguardhome'`
- Update: bump the image tag in `docker-compose.yml`, re-run `setup.sh`.
- Remove: `ssh "$PI_SSH" 'cd ~/adguard && docker compose down'` and reset the router DNS.
