#!/usr/bin/env bash
# Create or update named AdGuard Home clients from clients.json (name + IP + tags), so the
# dashboard and the phone app show a person's phone instead of a bare IP.
# IPs are pinned with DHCP reservations on the router. clients.json is git-ignored (real names +
# IPs); see clients.example.json for the format.
#
# Usage: AdGuardHome/clients.sh   (reads .env, see .env.example)
set -euo pipefail
cd "$(dirname "$0")"
. ./env.sh
[ -f clients.json ] || { echo "create clients.json (see clients.example.json)"; exit 1; }
API="http://$PI_IP:3000"
python3 - "$API" <<'PY'
import base64, json, os, sys, urllib.request
api = sys.argv[1]
auth = "Basic " + base64.b64encode(f"{os.environ['AGH_USER']}:{os.environ['AGH_PASSWORD']}".encode()).decode()
def call(path, body=None):
    req = urllib.request.Request(api + path, data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": auth, "Content-Type": "application/json"},
                                 method="POST" if body is not None else "GET")
    with urllib.request.urlopen(req) as r:
        txt = r.read().decode()
        return json.loads(txt) if txt.strip().startswith(("{", "[")) else txt
existing = {c["name"]: c for c in (call("/control/clients").get("clients") or [])}
by_ip = {i: c["name"] for c in existing.values() for i in c["ids"]}
for d in json.load(open("clients.json")):
    client = {"name": d["name"], "ids": [d["ip"]], "tags": d.get("tags", []),
              "use_global_settings": True, "use_global_blocked_services": True,
              "filtering_enabled": True, "safebrowsing_enabled": True, "parental_enabled": False,
              "upstreams": []}
    old = existing.get(d["name"]) or existing.get(by_ip.get(d["ip"], ""))
    if old:
        call("/control/clients/update", {"name": old["name"], "data": client}); print("updated", d["name"])
    else:
        call("/control/clients/add", client); print("added  ", d["name"])
PY
