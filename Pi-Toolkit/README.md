# Pi Toolkit — init0xff.com Cloudflare Tunnel bootstrap

Rebuilds the Raspberry Pi's local software (Docker + cloudflared) from
scratch and reconnects it to the existing, stable Cloudflare Tunnel
(`pi-init0xff`). Safe to re-run any time the Pi itself is rebuilt/reflashed —
it never touches anything on the Cloudflare side (zone, tunnel, DNS records
all stay exactly as configured).

## What it does
1. Removes any old Docker containers/images and the legacy `/root/homelab`
   reverse-proxy stack, if present (no-ops cleanly if there's nothing there).
2. Installs Docker, enables it, adds your user to the `docker` group.
3. Installs `cloudflared` from Cloudflare's official apt repo.
4. Installs `cloudflared` as a systemd service pointed at the existing
   tunnel via its token — no local `config.yml` needed, since this tunnel
   is dashboard-managed.

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
No script needed for this part — it's the whole point of the dashboard-managed
tunnel. Go to **Zero Trust dashboard → Networks → Tunnels → `pi-init0xff` →
Public Hostname → Add a public hostname**, enter the subdomain (under
`init0xff.com`) and the local `IP:port` on your home network, save. It's live
within seconds, with TLS handled automatically by Cloudflare.
