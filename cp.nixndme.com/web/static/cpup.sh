#!/usr/bin/env bash
# cpup: create a cp share from the terminal. Needs bash and curl.
#
#   echo hello | cpup                 text from stdin
#   cpup report.pdf photo.jpg         files (one share)
#   cpup https://example.com/page     link
#   cpup -t "deploy log" -e 1h -p < log.txt
#
# Setup: make an API token in cp under Settings > API tokens, then run `cpup --setup`
# (or set CP_URL and CP_TOKEN). The token can only create shares.
set -eu

CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/cpup/config"
MAX_RETRIES=4

usage() {
  cat <<'EOF'
Usage: cpup [options] [FILE | URL]...
       command | cpup [options]

Prints the link of the new share. With no FILE or URL, the share is the text read from stdin.
Use "-" as a FILE to add stdin as the text next to files or links.

Options:
  -t, --title TITLE     name of the share
  -e, --expires TIME    10m, 1h, 6h, 1d, 7d, 30d, 90d … (default: from cp Settings)
  -p, --public          anyone with the link can open it
  -P, --private         only you (logged in) can open it
  -b, --burn            delete after the first open
  -m, --max N           allow at most N downloads
  -w, --password        ask for a share password (typed, not shown)
  -L, --lang NAME       highlight the text as NAME (yaml, go, bash …)
  -l, --link URL        add a link (repeatable; bare http(s):// arguments work too)
      --setup           save the cp address and API token in ~/.config/cpup/config
  -h, --help            this help
Environment: CP_URL and CP_TOKEN override the config file.
EOF
}

die() { printf 'cpup: %s\n' "$*" >&2; exit 1; }

# JSON string literal for any shell string.
json_str() {
  local s
  s=$(printf '%s' "$1" | tr -d '\000-\010\013\014\016-\037')
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}
  s=${s//$'\r'/\\r}
  s=${s//$'\t'/\\t}
  printf '"%s"' "$s"
}

# Value of a "key" in a small JSON reply: a string (without escapes) or a number.
json_get() {
  local v
  v=$(printf '%s' "$1" | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p" | head -n 1)
  [ -n "$v" ] || v=$(printf '%s' "$1" | sed -n "s/.*\"$2\":\([0-9][0-9]*\).*/\1/p" | head -n 1)
  printf '%s' "$v"
}

setup() {
  local url token
  printf 'cp address (e.g. https://cp.example.com): ' >&2
  read -r url
  printf 'API token (cpt_…, input hidden): ' >&2
  read -rs token
  printf '\n' >&2
  case "$token" in cpt_*) ;; *) die "a token starts with cpt_" ;; esac
  check_url "${url%/}"
  mkdir -p "$(dirname "$CONFIG")"
  (umask 077 && printf 'CP_URL=%s\nCP_TOKEN=%s\n' "${url%/}" "$token" > "$CONFIG")
  chmod 600 "$CONFIG" # an older file keeps its mode otherwise
  printf 'Saved to %s\n' "$CONFIG" >&2
}

load_config() {
  local k v url="${CP_URL:-}" token="${CP_TOKEN:-}"
  if [ -f "$CONFIG" ]; then
    while IFS='=' read -r k v; do
      case "$k" in
        CP_URL) [ -n "$url" ] || url=$v ;;
        CP_TOKEN) [ -n "$token" ] || token=$v ;;
      esac
    done < "$CONFIG"
  fi
  [ -n "$url" ] && [ -n "$token" ] || die "not set up: run cpup --setup (or set CP_URL and CP_TOKEN)"
  BASE=${url%/}
  check_url "$BASE"
  case "$token" in cpt_*) ;; *) die "the token should start with cpt_ (see cpup --setup)" ;; esac
  # The token goes to curl through a private config file (-K), so it never shows in the
  # process list.
  printf 'header = "Authorization: Bearer %s"\n' "$token" > "$HDR"
}

# The token and everything shared would cross the network in the clear over http://.
check_url() {
  case "$1" in
    https://*) ;;
    http://localhost|http://localhost:*|http://localhost/*|http://127.0.0.1|http://127.0.0.1:*|http://127.0.0.1/*) ;;
    *) die "the cp address must start with https:// (got '$1')" ;;
  esac
}

# expiry_seconds 90m -> 5400
expiry_seconds() {
  local n=${1%[smhdSMHD]} unit=${1##*[0-9]}
  case "$n" in ''|*[!0-9]*) die "bad expiry '$1' (use e.g. 30m, 6h, 7d)" ;; esac
  case "$unit" in
    ''|s|S) echo "$n" ;;
    m|M) echo $((n * 60)) ;;
    h|H) echo $((n * 3600)) ;;
    d|D) echo $((n * 86400)) ;;
  esac
}

# call METHOD PATH [curl args…]: sets REPLY_BODY and REPLY_STATUS.
call() {
  local method=$1 path=$2 out
  shift 2
  out=$(curl -sS -K "$HDR" -X "$method" -w '\n%{http_code}' "$@" "$BASE$path") || return 1
  REPLY_STATUS=${out##*$'\n'}
  REPLY_BODY=${out%$'\n'*}
}

fail_reply() {
  local msg
  msg=$(json_get "$REPLY_BODY" error)
  die "${1}: ${msg:-HTTP $REPLY_STATUS}"
}

mime_of() {
  local m=''
  if command -v file >/dev/null 2>&1; then m=$(file -b --mime-type "$1" 2>/dev/null || true); fi
  printf '%s' "${m:-application/octet-stream}"
}

# upload FILE NAME MIME: prints the upload id.
upload() {
  local file=$1 name=$2 mime=$3 size id chunk i=0 off=0 try
  size=$(wc -c < "$file" | tr -d ' ')
  [ "$size" -gt 0 ] || die "$name is empty"
  call POST /api/uploads -H 'Content-Type: application/json' \
    --data-binary "{\"name\":$(json_str "$name"),\"size\":$size,\"mime\":$(json_str "$mime")}" || die "cannot reach $BASE"
  [ "$REPLY_STATUS" = 201 ] || fail_reply "$name"
  id=$(json_get "$REPLY_BODY" id)
  chunk=$(json_get "$REPLY_BODY" chunk_bytes)
  [ -n "$id" ] && [ -n "$chunk" ] || die "unexpected reply from $BASE"
  while [ "$off" -lt "$size" ]; do
    try=0
    while :; do
      dd if="$file" of="$TMP/chunk" bs="$chunk" skip="$i" count=1 2>/dev/null || die "$name: cannot read"
      if call PUT "/api/uploads/$id/$i" -H 'Content-Type: application/octet-stream' --data-binary @"$TMP/chunk"; then
        case "$REPLY_STATUS" in
          200) break ;;
          5*|429) ;; # server busy or restarting: retry
          *) fail_reply "$name" ;;
        esac
      fi
      try=$((try + 1))
      [ "$try" -le "$MAX_RETRIES" ] || die "$name: upload failed after $MAX_RETRIES retries"
      sleep "$try"
    done
    i=$((i + 1))
    off=$((off + chunk))
    if [ -t 2 ]; then
      printf '\r%s: %d%%' "$name" $(( (off < size ? off : size) * 100 / size )) >&2
    fi
  done
  [ -t 2 ] && printf '\n' >&2
  printf '%s' "$id"
}

main() {
  local title='' expires='' public='' burn='' max='' password='' lang='' use_stdin='' a
  local -a files=() links=()
  while [ $# -gt 0 ]; do
    case "$1" in
      -h|--help) usage; exit 0 ;;
      --setup) setup; exit 0 ;;
      -t|--title) title=${2?missing title}; shift ;;
      -e|--expires) expires=$(expiry_seconds "${2?missing expiry}"); shift ;;
      -p|--public) public=true ;;
      -P|--private) public=false ;;
      -b|--burn) burn=true ;;
      -m|--max) max=${2?missing number}; shift
        case "$max" in ''|*[!0-9]*) die "--max needs a number" ;; esac ;;
      -w|--password)
        printf 'Share password: ' >&2; read -rs password < /dev/tty; printf '\n' >&2
        [ -n "$password" ] || die "empty password" ;;
      -L|--lang) lang=${2?missing language}; shift ;;
      -l|--link) links+=("${2?missing link}"); shift ;;
      -) use_stdin=1 ;;
      --) shift; files+=("$@"); break ;;
      -*) die "unknown option $1 (see cpup --help)" ;;
      http://*|https://*) links+=("$1") ;;
      *) files+=("$1") ;;
    esac
    shift
  done
  if [ ${#files[@]} -eq 0 ] && [ ${#links[@]} -eq 0 ]; then
    [ -t 0 ] && { usage >&2; exit 2; }
    use_stdin=1
  fi
  for a in "${files[@]+"${files[@]}"}"; do
    [ -f "$a" ] && [ -r "$a" ] || die "$a: not a readable file"
  done

  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  trap 'exit 130' INT TERM HUP # exit runs the EXIT trap, so Ctrl-C leaves nothing behind
  HDR="$TMP/headers"
  (umask 077 && : > "$HDR")
  load_config

  local text_id='' uploads='' sep='' id
  if [ -n "$use_stdin" ]; then
    cat > "$TMP/paste.txt"
    text_id=$(upload "$TMP/paste.txt" paste.txt text/plain)
  fi
  for a in "${files[@]+"${files[@]}"}"; do
    id=$(upload "$a" "$(basename "$a")" "$(mime_of "$a")")
    uploads="$uploads$sep\"$id\""
    sep=,
  done
  local link_json='' lsep=''
  for a in "${links[@]+"${links[@]}"}"; do
    link_json="$link_json$lsep$(json_str "$a")"
    lsep=,
  done

  local body="{\"title\":$(json_str "$title"),\"text_upload\":\"$text_id\",\"uploads\":[$uploads],\"links\":[$link_json]"
  [ -n "$text_id" ] && [ -n "$lang" ] && body="$body,\"language\":$(json_str "$lang")"
  [ -n "$expires" ] && body="$body,\"expires_in\":$expires"
  [ -n "$public" ] && body="$body,\"public\":$public"
  [ -n "$burn" ] && body="$body,\"burn\":true"
  [ -n "$max" ] && body="$body,\"max_downloads\":$max"
  [ -n "$password" ] && body="$body,\"password\":$(json_str "$password")"
  body="$body}"

  printf '%s' "$body" > "$TMP/share.json"
  call POST /api/shares -H 'Content-Type: application/json' --data-binary @"$TMP/share.json" || die "cannot reach $BASE"
  [ "$REPLY_STATUS" = 201 ] || fail_reply "create share"
  json_get "$REPLY_BODY" url
  printf '\n'
}

main "$@"
