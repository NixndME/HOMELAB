# Pi Toolkit — init0xff.com Cloudflare Tunnel bootstrap

Rebuilds the Raspberry Pi's local software (Docker + cloudflared) from
scratch and reconnects it to the existing, stable Cloudflare Tunnel
(`pi-init0xff`). Safe to re-run any time the Pi itself is rebuilt/reflashed —
it never touches anything on the Cloudflare side (zone, tunnel, DNS records
all stay exactly as configured).

## What it does
1. Disables WiFi (`nmcli radio wifi off`) if the Pi is wired. Running both
   eth0 and wlan0 at once causes dual default routes, which caused real
   intermittent tunnel instability and firewall rules silently not matching
   depending on which interface traffic egressed from. Diagnosed the hard
   way — see commit history.
2. Sets explicit DNS (1.1.1.1 / 8.8.8.8) on the wired connection. A fresh
   Pi OS image can leave `/etc/resolv.conf` empty with `systemd-resolved`
   inactive, which makes cloudflared fall back to a much slower internal
   DNS path on every single request — this looks like generic "slowness"
   with no obvious cause otherwise.
3. Removes any old Docker containers/images and the legacy `/root/homelab`
   reverse-proxy stack, if present (no-ops cleanly if there's nothing there).
4. Installs Docker, enables it, adds your user to the `docker` group.
5. Installs `cloudflared` from Cloudflare's official apt repo.
6. Installs `cloudflared` as a systemd service pointed at the existing
   tunnel via its token — no local `config.yml` needed, since this tunnel
   is dashboard-managed.

If things are still slow after this script runs clean, check `ethtool -S eth0`
for `rx_ip_header_checksum_errors` — a high/climbing count means a bad cable
or port, not a software problem (this bit us once: 21k+ errors, 50%+ packet
loss, nothing fixable over SSH).

## What it does NOT do
- Does not create a new tunnel, zone, or DNS record. Those already exist
  and are managed from the Cloudflare Zero Trust dashboard.
- Does not need Route53/Hostinger touched again.

## Usage
```bash
export CF_TUNNEL_TOKEN="<paste tunnel token here>"
sudo -E ./setup.sh
```

Get the token from: **Zero Trust dashboard → Networks → Tunnels →
`pi-init0xff` → "Install and run a connector" → copy the token.**
Never commit the token itself to git — export it as an env var each time.

## Adding a new public service afterward
The dashboard's "Hostname routes" tab looked like self-service but doesn't
actually wire up DNS + ingress by itself (its own UI warns about this). The
reliable path: set the tunnel's ingress config and DNS record together via
the Cloudflare API (two calls — `PUT .../cfd_tunnel/{id}/configurations` for
ingress, `POST .../dns_records` for the CNAME to `{tunnel_id}.cfargotunnel.com`).
Ask Claude to do this, or script it yourself with the same two calls.
