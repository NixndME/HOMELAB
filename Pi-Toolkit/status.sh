#!/usr/bin/env bash
# Read-only health check for the Pi: run it after a power cut or any change.
# Exits non-zero if something would not come back by itself or is not running.
#
# Usage: ./status.sh   (as a user in the docker group)

set -uo pipefail

problems=0
warn() { echo "  !! $*"; problems=$((problems + 1)); }

echo "== Boot"
for unit in docker.service containerd.service; do
  [ "$(systemctl is-enabled "$unit" 2>/dev/null)" = enabled ] && echo "  ok  $unit enabled at boot" || warn "$unit is not enabled at boot"
done
echo "  up  $(uptime -p)"

echo "== Containers (name | state | health | restart policy | compose dir)"
while read -r id; do
  info=$(docker inspect -f '{{.Name}}|{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}}|{{.HostConfig.RestartPolicy.Name}}|{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$id")
  IFS='|' read -r name state health policy dir <<<"$info"
  echo "  ${name#/} | $state | $health | $policy | ${dir:-not compose}"
  [ "$state" = running ] || warn "${name#/} is $state"
  [ "$health" = unhealthy ] && warn "${name#/} is unhealthy"
  case "$policy" in always|unless-stopped) ;; *) warn "${name#/} restart policy '$policy' will not survive a reboot" ;; esac
done < <(docker ps -aq)

echo "== Timers"
systemctl list-timers --all --no-pager --no-legend '*update*' 2>/dev/null | sed 's/^/  /'

echo "== Disk and memory"
df -h / | awk 'NR==2{print "  disk " $5 " used, " $4 " free"}'
free -m | awk '/Mem:/{print "  mem  " $7 " MiB available of " $2}'

if [ "$problems" -eq 0 ]; then echo "== all good"; else echo "== $problems problem(s)"; exit 1; fi
