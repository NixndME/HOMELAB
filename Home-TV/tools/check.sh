#!/usr/bin/env bash
# Report drift from a debloat file: listed packages that are enabled again, and whether the custom
# launcher is still home (firmware updates undo both). Exits 1 on drift. --fix re-applies.
#
# Usage: TV_HOST=<TV_IP> tools/check.sh examples/xiaomi-mitv-mooq0/debloat.txt [--fix]
set -euo pipefail
cd "$(dirname "$0")/.."
LIST="${1:?usage: check.sh <debloat list> [--fix]}"
FIX="${2:-}"
HOME_PKG="${HOME_PKG:-com.gathin.tvhome}"
STOCK="${STOCK_LAUNCHER:-com.google.android.tvlauncher}"
PY=tools/venv/bin/python
[ -x "$PY" ] || PY=tools/venv/Scripts/python.exe
export MSYS_NO_PATHCONV=1
[ -x "$PY" ] || { echo "run: python3 -m venv tools/venv && tools/venv/bin/pip install -r tools/requirements.txt"; exit 1; }

STATE=$(printf '%s\n' \
  "pm list packages -d | sed 's/^/D /'" \
  "pm list packages | sed 's/^/I /'" \
  "cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.HOME | tail -1 | sed 's/^/H /'" \
  | "$PY" -I tools/tv.py --batch | tr -d '\r')

drift=0
while read -r pkg; do
  if ! grep -qx "I package:$pkg" <<<"$STATE"; then
    echo "  absent    $pkg"
  elif ! grep -qx "D package:$pkg" <<<"$STATE"; then
    echo "  ENABLED   $pkg"; drift=1
  fi
done < <(grep -vE '^\s*(#|$)' "$LIST" | sed -E 's/\s+#.*$//' | tr -d '[:blank:]\r' | grep -v '^$' | sort -u)

HOME_NOW=$(sed -n 's/^H //p' <<<"$STATE")
if [[ "$HOME_NOW" != "$HOME_PKG/"* ]]; then
  echo "  HOME      is '${HOME_NOW:-unset}', expected $HOME_PKG"; drift=1
fi
if grep -qx "I package:$STOCK" <<<"$STATE" && ! grep -qx "D package:$STOCK" <<<"$STATE"; then
  echo "  ENABLED   $STOCK (stock launcher)"; drift=1
fi

if [ "$drift" = 0 ]; then echo "OK: no drift"; exit 0; fi
if [ "$FIX" != "--fix" ]; then echo "Drift found. Re-run with --fix to re-apply."; exit 1; fi

tools/disable.sh "$LIST" >/dev/null
printf '%s\n' \
  "cmd package set-home-activity --user 0 $HOME_PKG" \
  "pm disable-user --user 0 $STOCK" \
  | "$PY" -I tools/tv.py --batch >/dev/null
echo "Re-applied. Checking again:"
exec "$0" "$LIST"
