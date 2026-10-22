#!/usr/bin/env bash
# Iki caddy binary'sini ayni senaryolarda karsilastirir. Varsayilan olarak
# caddy-ip-list'in bilinen-iyi (eski) ve mevcut (yeni) surumlerini derler.
#
# Kullanim:
#   ./scripts/compare.sh              # iki surumu derle + kos
#   ./scripts/compare.sh --json       # JSON cikti
#   OLD_BIN=... NEW_BIN=... ./scripts/compare.sh   # hazir binary'lerle
#
# Sabitler surum degistikce guncellenir; eski surum "kanitlanmis kotu davranis"
# referansidir, degistirmeyin.
set -uo pipefail
HERE=$(cd "$(dirname "$0")/.." && pwd)
OLD_REF=${OLD_REF:-9336c172e0f6}   # Provision senkron fetch; 404 + cache yok = config reddedilir
NEW_REF=${NEW_REF:-0d8b2ae}          # cache-first + file kaynagi + bos liste korumasi + stat kisa devre
JSON=0; [ "${1:-}" = "--json" ] && JSON=1

build() { # $1=ref $2=output
  local ref=$1 out=$2
  [ -x "$out" ] && return 0
  echo "derleniyor: $ref -> $(basename "$out")" >&2
  XCADDY=$(command -v xcaddy 2>/dev/null || echo "$HOME/go/bin/xcaddy")
  "$XCADDY" build v2.11.2 \
    --with "github.com/monobilisim/caddy-ip-list@$ref" \
    --with github.com/tuzzmaniandevil/caddy-dynamic-clientip@v1.0.4 \
    --output "$out" >/dev/null 2>&1 || { echo "derleme basarisiz: $ref" >&2; return 1; }
}

BUILDDIR=$(mktemp -d /tmp/iplist-bin.XXXXXX)
OLD=${OLD_BIN:-$BUILDDIR/caddy-old}
NEW=${NEW_BIN:-$BUILDDIR/caddy-new}
if [ -z "${OLD_BIN:-}" ] || [ -z "${NEW_BIN:-}" ]; then
  command -v xcaddy >/dev/null 2>&1 || [ -x "$HOME/go/bin/xcaddy" ] || { echo "xcaddy gerekli (go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest)" >&2; exit 1; }
  build "$OLD_REF" "$OLD" || exit 1
  build "$NEW_REF" "$NEW" || exit 1
fi

WORK=$(mktemp -d /tmp/iplist-e2e.XXXXXX)
BASE=$(( 20000 + RANDOM % 20000 ))   # rastgele taban: bayat sureclerin portlarini kullanmayalim
N=0
declare -a ROWS
PIDS=()

cleanup() {
  for p in "${PIDS[@]:-}"; do kill -9 "$p" >/dev/null 2>&1; done
  rm -rf "$WORK" "$BUILDDIR"
}
trap cleanup EXIT

write_src() { # $1=port $2=mode-file
  cat > "$WORK/src-$1.py" <<PY
import http.server, socketserver
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if open("$2").read().strip() == "404":
            self.send_error(404); return
        body = b"127.0.0.1/32\n10.0.0.0/8\n"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
socketserver.ThreadingTCPServer(("127.0.0.1", $1), H).serve_forever()
PY
}

wait_admin() { # $1=admin port; 0 = config yuklendi
  for _ in $(seq 1 100); do
    curl -sf -o /dev/null -m 1 "localhost:$1/config/" 2>/dev/null && return 0
    sleep 0.2
  done
  return 1
}

run_case() { # $1=etiket $2=binary $3=404|200 $4=cache 1|0
  local label=$1 bin=$2 srcmode=$3 with_cache=$4
  N=$((N+1))
  local sport=$((BASE + N*10)) admin=$((BASE + N*10 + 3)) http=$((BASE + N*10 + 5))
  local dir="$WORK/$label"; mkdir -p "$dir"
  local mode="$dir/mode"; echo "$srcmode" > "$mode"
  local cache="$dir/cache.json"

  if [ "$with_cache" = "1" ]; then
    python3 -c "
import json,sys,datetime
json.dump({'prefixes':['10.0.0.0/8'],'updated_at':datetime.datetime.now().astimezone().isoformat()},open(sys.argv[1],'w'))" "$cache"
  fi

  write_src "$sport" "$mode"
  python3 "$WORK/src-$sport.py" & PIDS+=($!)
  sleep 1

  cat > "$dir/Caddyfile" <<EOF
{
	admin localhost:$admin
	http_port $http
	auto_https off
	storage file_system $dir/st
}
http://t.test {
	@allowed dynamic_client_ip list {
		url http://127.0.0.1:$sport/list.txt
		interval 2s
		timeout 3s
		retries 1
		cache_file $cache
	}
	respond @allowed "GECIT" 200
	respond "BLOK" 403
}
EOF

  "$bin" run --adapter caddyfile --config "$dir/Caddyfile" > "$dir/run.log" 2>&1 &
  local cpid=$!; PIDS+=($cpid)

  local up=0
  wait_admin "$admin" && up=1

  local code="-"
  [ "$up" = "1" ] && code=$(curl -s -o /dev/null -m 3 -w '%{http_code}' -H 'Host: t.test' "http://127.0.0.1:$http/")

  # 404 -> 200: kaynagi duzelt, servisin toparlanmasini olc
  local recover="-"
  if [ "$srcmode" = "404" ] && [ "$up" = "1" ]; then
    echo 200 > "$mode"
    for i in $(seq 1 40); do
      if [ "$(curl -s -o /dev/null -m 2 -w '%{http_code}' -H 'Host: t.test' "http://127.0.0.1:$http/")" = "200" ]; then
        recover="$((i * 250))ms"; break
      fi
      sleep 0.25
    done
  fi

  local err=""; [ "$up" = "0" ] && err=$(grep -oE "failed to fetch initial IP ranges[^\"]{0,60}" "$dir/run.log" | head -1)
  [ "$up" = "0" ] && [ -z "$err" ] && err="(config yuklenmedi)"

  local cachesz="yok"
  [ -f "$cache" ] && cachesz=$(python3 -c "import json,sys;print(len(json.load(open(sys.argv[1]))['prefixes']))" "$cache" 2>/dev/null || echo "bozuk")

  kill -9 "$cpid" >/dev/null 2>&1
  wait "$cpid" 2>/dev/null

  ROWS+=("$label|$up|$code|$recover|$cachesz|$err")
  printf "  %-20s up=%s http=%-4s 404->200=%-7s cache=%s %s\n" \
    "$label" "$up" "$code" "$recover" "$cachesz" "${err:0:45}" >&2
}

echo "== eski: $("$OLD" build-info 2>/dev/null | awk -F'\t' '/^dep\tgithub.com\/monobilisim/{print $3}')" >&2
echo "== yeni: $("$NEW" build-info 2>/dev/null | awk -F'\t' '/^dep\tgithub.com\/monobilisim/{print $3}')" >&2
run_case "eski-404-cache-yok" "$OLD" 404 0
run_case "eski-404-cache-var" "$OLD" 404 1
run_case "eski-200-cache-yok" "$OLD" 200 0
run_case "yeni-404-cache-yok" "$NEW" 404 0
run_case "yeni-404-cache-var" "$NEW" 404 1
run_case "yeni-200-cache-yok" "$NEW" 200 0

if [ "$JSON" = "1" ]; then
  printf '['
  first=1
  for r in "${ROWS[@]}"; do
    IFS='|' read -r l u c rec ca e <<< "$r"
    [ $first = 1 ] || printf ','
    first=0
    printf '{"case":"%s","config_loaded":%s,"http":"%s","recovery":"%s","cache_prefixes":"%s","error":"%s"}' \
      "$l" "$u" "$c" "$rec" "$ca" "${e//\"/}"
  done
  printf ']\n'
else
  echo
  echo "| senaryo | config yüklendi | HTTP | 404→200 toparlanma | cache prefix | hata |"
  echo "|---|---|---|---|---|---|"
  for r in "${ROWS[@]}"; do
    IFS='|' read -r l u c rec ca e <<< "$r"
    loaded=$([ "$u" = "1" ] && echo "evet" || echo "**HAYIR**")
    printf '| `%s` | %s | %s | %s | %s | %s |\n' "$l" "$loaded" "$c" "$rec" "$ca" "${e:0:45}"
  done
fi
