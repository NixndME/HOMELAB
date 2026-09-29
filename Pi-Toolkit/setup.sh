#!/usr/bin/env bash
# Idempotent Raspberry Pi bootstrap for the init0xff.com Cloudflare Tunnel setup.
#
# Re-run this any time you rebuild the Pi (fresh OS flash, reset SD card, etc).
# It never touches Cloudflare's side (zone, tunnel, DNS) - that's already
# created once and stays stable. This script only rebuilds the LOCAL software:
# Docker + cloudflared, connected to the existing tunnel via its token.
#
# Usage:
#   export CF_TUNNEL_TOKEN="<token from Cloudflare Zero Trust dashboard>"
#   sudo -E ./setup.sh
#
# Get the token from: Zero Trust dashboard -> Networks -> Tunnels -> pi-init0xff
# -> "Install and run a connector" -> copy the token (long string after --token).

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "Run as root: sudo -E ./setup.sh" >&2
  exit 1
fi

if [ -z "${CF_TUNNEL_TOKEN:-}" ]; then
  echo "CF_TUNNEL_TOKEN is not set. Export it first (see header of this script)." >&2
  exit 1
fi

echo "== Disabling WiFi (Ethernet-only) =="
# Running both eth0 and wlan0 simultaneously causes dual default routes and
# intermittent connection instability (found the hard way: random tunnel
# drops and firewall rules silently not matching depending on which
# interface traffic happened to egress from). If this Pi is wired, WiFi
# should stay off.
if command -v nmcli >/dev/null 2>&1; then
  nmcli radio wifi off || true
fi

echo "== Ensuring DNS is actually configured =="
# A fresh Pi OS image / NetworkManager profile can leave /etc/resolv.conf
# empty with systemd-resolved inactive, which makes cloudflared fall back to
# a much slower internal DNS path on every single request. Set explicit
# resolvers on the wired connection so this can't silently regress.
ETH_CON=$(nmcli -t -f NAME,DEVICE con show --active 2>/dev/null | awk -F: '$2=="eth0"{print $1}' | head -1)
if [ -n "$ETH_CON" ]; then
  nmcli con modify "$ETH_CON" ipv4.dns "1.1.1.1 8.8.8.8" ipv4.ignore-auto-dns no || true
  nmcli con up "$ETH_CON" || true
fi

echo "== Removing any previous docker-based reverse proxy / homelab stack =="
if command -v docker >/dev/null 2>&1; then
  docker ps -aq | xargs -r docker rm -f
  docker images -q | xargs -r docker rmi -f
fi
rm -rf /root/homelab

echo "== Installing Docker (if not already present) =="
if ! command -v docker >/dev/null 2>&1; then
  curl -fsSL https://get.docker.com | sh
fi
systemctl enable --now docker

REAL_USER="${SUDO_USER:-root}"
if [ "$REAL_USER" != "root" ]; then
  usermod -aG docker "$REAL_USER"
fi

echo "== Installing cloudflared (if not already present) =="
if ! command -v cloudflared >/dev/null 2>&1; then
  mkdir -p --mode=0755 /usr/share/keyrings
  curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
  OS_CODENAME=$(. /etc/os-release && echo "$VERSION_CODENAME")
  echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared ${OS_CODENAME} main" \
    > /etc/apt/sources.list.d/cloudflared.list
  apt-get update -qq
  apt-get install -y cloudflared
fi

echo "== Connecting cloudflared to the existing tunnel (remote-managed, no local config.yml) =="
# Wipe any previous service pointing at a different/old token before reinstalling.
cloudflared service uninstall 2>/dev/null || true
cloudflared service install "$CF_TUNNEL_TOKEN"
systemctl enable --now cloudflared

echo "== Done. Check status with: systemctl status cloudflared =="
echo "== Manage public hostnames from: Zero Trust dashboard -> Networks -> Tunnels -> pi-init0xff -> Public Hostname =="
