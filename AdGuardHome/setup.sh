#!/usr/bin/env bash
# Deploy and configure AdGuard Home on the Pi from this machine. Safe to re-run.
#
# Only touches ~/adguard on the Pi and the "adguard" compose project. It never restarts
# Docker, never touches other containers (e.g. a production app on the same Pi), never changes
# the Pi's own DNS/network config, and binds DNS only to the Pi's LAN IP.
#
# Usage: cp .env.example .env  (fill in), then  AdGuardHome/setup.sh
set -euo pipefail
cd "$(dirname "$0")"
. ./env.sh
PI="$PI_SSH"
API="http://$PI_IP:3000"

echo "== Deploying compose project to $PI:~/adguard"
ssh "$PI" 'mkdir -p ~/adguard/work ~/adguard/conf'
scp -q docker-compose.yml "$PI:adguard/docker-compose.yml"
ssh "$PI" 'cd ~/adguard && docker compose pull -q && docker compose up -d'

echo "== Waiting for AdGuard Home on :3000"
for _ in $(seq 1 30); do curl -s -o /dev/null "$API/" && break; sleep 2; done

# First run only: the setup wizard API. DNS binds to the LAN IP only (systemd-resolved owns
# 127.0.0.53:53); the dashboard uses 0.0.0.0:3000 because the wizard itself already holds :3000.
if curl -s "$API/control/install/get_addresses" | grep -q '"interfaces"'; then
  echo "== First run: completing setup wizard"
  curl -sSf -X POST "$API/control/install/configure" -H 'Content-Type: application/json' -d @- <<EOF
{"web":{"ip":"0.0.0.0","port":3000},"dns":{"ip":"$PI_IP","port":53},
 "username":"$AGH_USER","password":$(printf '%s' "$AGH_PASSWORD" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')}
EOF
  for _ in $(seq 1 30); do curl -s -o /dev/null -u "$AGH_USER:$AGH_PASSWORD" "$API/control/status" && break; sleep 2; done
fi

api() { curl -sSf -u "$AGH_USER:$AGH_PASSWORD" -H 'Content-Type: application/json' "$@"; }

echo "== Upstreams: encrypted DNS-over-HTTPS (Quad9 + Cloudflare), parallel, cached"
api -X POST "$API/control/dns_config" -d '{
  "upstream_dns": ["https://dns.quad9.net/dns-query", "https://cloudflare-dns.com/dns-query"],
  "bootstrap_dns": ["9.9.9.9", "1.1.1.1"],
  "fallback_dns": ["9.9.9.9", "1.1.1.1"],
  "upstream_mode": "parallel",
  "cache_size": 8388608, "cache_optimistic": true,
  "ratelimit": 0, "blocking_mode": "default", "dnssec_enabled": true
}'

echo "== Blocklists"
add_list() {
  api -s -X POST "$API/control/filtering/add_url" -d "{\"name\":\"$1\",\"url\":\"$2\",\"whitelist\":false}" >/dev/null \
    && echo "   added: $1" || echo "   present: $1"
}
add_list "HaGeZi Multi NORMAL" "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/multi.txt"
add_list "HaGeZi Threat Intelligence (mini)" "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/tif.mini.txt"
# AdGuard DNS filter is enabled by default on first run.
api -X POST "$API/control/filtering/config" -d '{"enabled":true,"interval":24}'
curl -sSf -u "$AGH_USER:$AGH_PASSWORD" -X POST "$API/control/safebrowsing/enable"   # no JSON content-type, else 415

echo "== Logs/stats: 7 days (keeps SD-card writes low)"
api -X PUT "$API/control/querylog/config/update" -d '{"enabled":true,"interval":604800000,"anonymize_client_ip":false,"ignored":[]}'
api -X PUT "$API/control/stats/config/update" -d '{"enabled":true,"interval":604800000,"ignored":[]}'

api -X POST "$API/control/filtering/refresh" -d '{"whitelist":false}' >/dev/null
echo "== Done. Dashboard: $API  (user: $AGH_USER)"
