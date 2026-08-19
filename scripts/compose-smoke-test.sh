#!/usr/bin/env bash
#
# Exercises the whole Docker Compose code path — argument construction, working
# directory, `ps --format json` parsing, container-to-port attribution, status
# derivation — against a stub `docker` CLI instead of a real daemon.
#
# It is not a substitute for testing against Docker, but it does catch the
# regressions that are otherwise invisible on a machine without a daemon, and it
# runs anywhere.
#
# Usage: scripts/compose-smoke-test.sh [path/to/devcc]

set -uo pipefail

BIN=${1:-$(cd "$(dirname "$0")/.." && pwd)/agent/bin/devcc}
WORK=$(mktemp -d)
PORT=47139
FAILURES=0

cleanup() {
  [ -f "$WORK/agent.pid" ] && kill "$(cat "$WORK/agent.pid")" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

pass() { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n     -> %s\n' "$1" "$2"; FAILURES=$((FAILURES + 1)); }

check() { # check <name> <expected> <actual>
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "expected '$2', got '$3'"; fi
}

# --- the stub docker CLI ----------------------------------------------------

mkdir -p "$WORK/bin"
cat > "$WORK/bin/docker" <<'STUB'
#!/usr/bin/env bash
# Minimal stand-in for the docker CLI. State lives in $DOCKER_STUB_STATE.
state="${DOCKER_STUB_STATE:-/tmp/docker-stub-state}"
project=$(basename "$PWD")

args=("$@")
sub="${args[0]:-}"

if [ "$sub" = "info" ]; then echo "27.0.0"; exit 0; fi

if [ "$sub" = "ps" ]; then
  # Host-side port ownership for running containers, plus one container from an
  # unrelated compose project squatting on 3999.
  if [ -f "$state/shop.up" ]; then
    echo '{"ID":"c1","Names":"shop-web-1","Ports":"0.0.0.0:3000->80/tcp, :::3000->80/tcp","Labels":"com.docker.compose.project=shop","Image":"nginx"}'
  fi
  echo '{"ID":"c9","Names":"stranger-web-1","Ports":"0.0.0.0:3999->80/tcp","Labels":"com.docker.compose.project=stranger","Image":"nginx"}'
  exit 0
fi

if [ "$sub" != "compose" ]; then exit 0; fi

# Skip global compose flags to find the verb.
i=1
while [ $i -lt ${#args[@]} ]; do
  case "${args[$i]}" in
    --file|-f|--project-name|-p|--ansi) i=$((i + 2));;
    -*) i=$((i + 1));;
    *) break;;
  esac
done
verb="${args[$i]:-}"

case "$verb" in
  version) echo "2.29.0"; exit 0;;
  up)      touch "$state/$project.up"; echo "Container ${project}-web-1  Started" >&2; exit 0;;
  down)    rm -f "$state/$project.up"; echo "Container ${project}-web-1  Removed" >&2; exit 0;;
  restart) echo "Container ${project}-web-1  Restarted" >&2; exit 0;;
  logs)    echo "web-1  | stub log line from $project"; exit 0;;
  ps)
    if [ -f "$state/$project.up" ]; then
      # NDJSON, the format compose v2.21+ emits.
      echo '{"ID":"c1","Name":"'"$project"'-web-1","Image":"nginx","Service":"web","State":"running","Status":"Up 3 seconds","Health":"","Project":"'"$project"'","Publishers":[{"URL":"0.0.0.0","TargetPort":80,"PublishedPort":3000,"Protocol":"tcp"}]}'
      echo '{"ID":"c2","Name":"'"$project"'-db-1","Image":"postgres","Service":"db","State":"running","Status":"Up 3 seconds","Health":"healthy","Project":"'"$project"'","Publishers":[{"URL":"0.0.0.0","TargetPort":5432,"PublishedPort":5432,"Protocol":"tcp"}]}'
    fi
    exit 0;;
esac
exit 0
STUB
chmod +x "$WORK/bin/docker"
mkdir -p "$WORK/state"

# --- fixture project --------------------------------------------------------

mkdir -p "$WORK/projects/shop"
cat > "$WORK/projects/shop/compose.yaml" <<'YAML'
name: shop
services:
  web:
    image: nginx
    ports: ["3000:80"]
  db:
    image: postgres
    ports: ["5432:5432"]
YAML

# A second project whose port is held by someone else's container.
mkdir -p "$WORK/projects/blog"
cat > "$WORK/projects/blog/compose.yaml" <<'YAML'
name: blog
services:
  web:
    image: nginx
    ports: ["3999:80"]
YAML

cat > "$WORK/config.yaml" <<YAML
listen: 127.0.0.1:$PORT
scan:
  roots: [$WORK/projects]
  depth: 2
poll:
  interval: 1s
YAML

# --- run --------------------------------------------------------------------

export PATH="$WORK/bin:$PATH"
export DOCKER_STUB_STATE="$WORK/state"

"$BIN" -config "$WORK/config.yaml" -state-dir "$WORK/agent" > "$WORK/agent.log" 2>&1 &
echo $! > "$WORK/agent.pid"
sleep 2

TOKEN=$("$BIN" token -state-dir "$WORK/agent")
api() {
  local method=$1 path=$2
  curl -s --noproxy '*' -X "$method" -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT$path"
}
id_of() { api GET /api/projects | jq -r ".projects[]|select(.name==\"$1\")|.id"; }

echo "compose smoke test (stub docker CLI)"

SHOP=$(id_of shop)
BLOG=$(id_of blog)
if [ -z "$SHOP" ] || [ -z "$BLOG" ]; then
  fail "discovery" "both compose projects should be discovered"
  exit 1
fi
pass "discovers compose projects"

check "compose file ports are parsed" \
  "3000,5432" \
  "$(api GET "/api/projects/$SHOP" | jq -r '[.ports[].port]|join(",")')"

check "stopped before start" "stopped" \
  "$(api GET "/api/projects/$SHOP/status" | jq -r .status)"

api POST "/api/projects/$SHOP/start" > /dev/null
sleep 2
AFTER=$(api GET "/api/projects/$SHOP")

check "running after start"   "running" "$(echo "$AFTER" | jq -r .state.status)"
check "container count"       "2 containers" "$(echo "$AFTER" | jq -r .state.detail)"
check "services listed"       "web,db" "$(echo "$AFTER" | jq -r '[.state.containers[].service]|join(",")')"
check "container health read" "healthy" "$(echo "$AFTER" | jq -r '.state.containers[]|select(.service=="db")|.health')"
check "own port attributed"   "owned" \
  "$(echo "$AFTER" | jq -r '.state.ports[]|select(.port==3000)|.state')"

# The interesting case: a port held by a *different* compose project.
BLOG_STATE=$(api GET "/api/projects/$BLOG")
check "foreign container is a conflict" "conflict" \
  "$(echo "$BLOG_STATE" | jq -r '.state.ports[]|select(.port==3999)|.state')"
check "conflict names the owner" "stranger" \
  "$(echo "$BLOG_STATE" | jq -r '.state.ports[]|select(.port==3999)|.owner.composeProject')"
check "conflict status" "conflict" "$(echo "$BLOG_STATE" | jq -r .state.status)"

check "logs come from containers" "web-1  | stub log line from shop" \
  "$(api GET "/api/projects/$SHOP/logs?tail=5" | jq -r '.lines[0].text')"

api POST "/api/projects/$SHOP/stop" > /dev/null
sleep 2
check "stopped after stop" "stopped" \
  "$(api GET "/api/projects/$SHOP/status" | jq -r .status)"
check "port released" "free" \
  "$(api GET "/api/projects/$SHOP" | jq -r '.state.ports[]|select(.port==3000)|.state')"

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "all compose smoke checks passed"
else
  echo "$FAILURES check(s) failed"
fi
exit "$FAILURES"
