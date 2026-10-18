# Sourced by setup.sh and clients.sh: loads site-specific settings from the git-ignored .env.
[ -f .env ] && { set -a; . ./.env; set +a; }
: "${PI_SSH:?set PI_SSH in .env (see .env.example)}"
: "${PI_IP:?set PI_IP in .env}"
: "${AGH_USER:?set AGH_USER in .env}"
if [ -z "${AGH_PASSWORD:-}" ]; then
  f="${AGH_PASSWORD_FILE/#\~/$HOME}"
  [ -n "$f" ] && [ -r "$f" ] || { echo "set AGH_PASSWORD or AGH_PASSWORD_FILE in .env"; exit 1; }
  AGH_PASSWORD="$(cat "$f")"
fi
export PI_SSH PI_IP AGH_USER AGH_PASSWORD
