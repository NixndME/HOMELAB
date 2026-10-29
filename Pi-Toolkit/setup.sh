#!/usr/bin/env bash
# Idempotent, non-destructive base setup for the home Raspberry Pi (Ubuntu or Raspberry Pi OS).
#
# Safe to re-run on the live Pi: it never stops, removes or recreates containers, images or
# volumes, and never edits the network config. It only makes sure the host can run the app
# stacks and brings them back after a power cut:
#   - Docker installed, and docker + containerd enabled at boot
#   - your user in the docker group
#   - WiFi blocked when the Pi is wired (two default routes caused tunnel drops before)
#   - persistent systemd journal, so logs from before a power cut survive
#
# Each app (cp, AdGuard Home, ...) is its own compose project in ~/<app> with
# `restart: always` and, if public, its own cloudflared container. See README.md.
#
# Usage: sudo ./setup.sh

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "Run as root: sudo ./setup.sh" >&2
  exit 1
fi

REAL_USER="${SUDO_USER:-root}"
WIRED_IF="${WIRED_IF:-eth0}"

echo "== Docker =="
if ! command -v docker >/dev/null 2>&1; then
  curl -fsSL https://get.docker.com | sh
else
  echo "already installed: $(docker --version)"
fi
# Containers with `restart: always` only come back after a power cut if the daemon starts at boot.
systemctl enable docker.service containerd.service
systemctl is-active --quiet docker || systemctl start docker
if [ "$REAL_USER" != "root" ] && ! id -nG "$REAL_USER" | grep -qw docker; then
  usermod -aG docker "$REAL_USER"
  echo "added $REAL_USER to the docker group (log in again to use it)"
fi

echo "== WiFi =="
# Running eth0 and wlan0 together gave two default routes: random tunnel drops and firewall
# rules matching only on one interface. If the cable is in, keep WiFi off (rfkill state persists).
if [ "$(cat "/sys/class/net/$WIRED_IF/carrier" 2>/dev/null || echo 0)" = "1" ] && command -v rfkill >/dev/null 2>&1; then
  rfkill block wifi
  echo "wired on $WIRED_IF: WiFi blocked"
else
  echo "no carrier on $WIRED_IF (or no rfkill): WiFi left as is"
fi

echo "== Persistent journal =="
if [ ! -d /var/log/journal ]; then
  mkdir -p /var/log/journal
  systemd-tmpfiles --create --prefix /var/log/journal
  systemctl restart systemd-journald
  echo "enabled"
else
  echo "already enabled"
fi

echo "== Done. Check every stack with: ./status.sh =="
