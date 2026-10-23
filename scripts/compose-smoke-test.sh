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
# Record every invocation so the test can assert on the arguments and the
# environment the agent passed (port overrides ride in both).
echo "ARGS: $* | WEB_PORT=${WEB_PORT:-} BLOG_PORT=${BLOG_PORT:-}" >> "$state/calls.log"

args=("$@")
sub="${args[0]:-}"

if [ "$sub" = "info" ]; then echo "27.0.0"; exit 0; fi

if [ "$sub" = "ps" ]; then
  # Host-side port ownership for running containers, plus one container from an
  # unrelated compose project squatting on 3999.
  if [ -f "$state/shop.up" ]; then
    webport="${WEB_PORT:-3000}"
    echo '{"ID":"c1","Names":"shop-web-1","Ports":"0.0.0.0:'"$webport"'->80/tcp, :::'"$webport"'->80/tcp","Labels":"com.docker.compose.project=shop","Image":"nginx"}'
  fi
  echo '{"ID":"c9","Names":"stranger-web-1","Ports":"0.0.0.0:3999->80/tcp","Labels":"com.docker.compose.project=stranger","Image":"nginx"}'
  exit 0
fi

if [ "$sub" != "compose" ]; then exit 0; fi

# Skip global compose flags to find the verb.
i=1
while [ $i -lt ${#args[@]} ]; do
  case "${args[$i]}" in
    --file|-f|--project-name|-p|--ansi|--project-directory) i=$((i + 2));;
    -*) i=$((i + 1));;
    *) break;;
  esac
done
verb="${args[$i]:-}"

case "$verb" in
  version) echo "2.29.0"; exit 0;;
  up)      touch "$state/$project.up"
           # A trailing service name means "only this one".
           svc="${args[$((i + 1))]:-}"
           case "$svc" in ""|-*) rm -f "$state/$project".*.down;; *) rm -f "$state/$project.$svc.down";; esac
           echo "Container ${project}-web-1  Started" >&2; exit 0;;
  stop)    svc="${args[${#args[@]}-1]}"; touch "$state/$project.$svc.down"
           echo "Container ${project}-${svc}-1  Stopped" >&2; exit 0;;
  down)    rm -f "$state/$project.up"; echo "Container ${project}-web-1  Removed" >&2; exit 0;;
  restart) echo "Container ${project}-web-1  Restarted" >&2; exit 0;;
  logs)    echo "web-1  | stub log line from $project"; exit 0;;
  ps)
    if [ -f "$state/$project.up" ]; then
      # NDJSON, the format compose v2.21+ emits.
      webport="${WEB_PORT:-3000}"
      webstate=running; webstatus="Up 3 seconds"
      [ -f "$state/$project.web.down" ] && { webstate=exited; webstatus="Exited (0)"; }
      dbstate=running
      [ -f "$state/$project.db.down" ] && dbstate=exited
      echo '{"ID":"c1","Name":"'"$project"'-web-1","Image":"nginx","Service":"web","State":"'"$webstate"'","Status":"'"$webstatus"'","Health":"","Project":"'"$project"'","Publishers":[{"URL":"0.0.0.0","TargetPort":80,"PublishedPort":'"$webport"',"Protocol":"tcp"},{"URL":"::","TargetPort":80,"PublishedPort":'"$webport"',"Protocol":"tcp"}]}'
      echo '{"ID":"c2","Name":"'"$project"'-db-1","Image":"postgres","Service":"db","State":"'"$dbstate"'","Status":"Up 3 seconds","Health":"healthy","Project":"'"$project"'","Publishers":[{"URL":"0.0.0.0","TargetPort":5432,"PublishedPort":5432,"Protocol":"tcp"}]}'
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
    ports: ["${WEB_PORT:-3000}:80"]
  db:
    image: postgres
    ports: ["5432:5432"]
  worker:
    image: alpine
    command: ["sleep", "60"]
YAML

# Most real projects keep something in an override sibling. Compose loads it
# automatically only when no -f is passed, and the agent always passes -f.
cat > "$WORK/projects/shop/compose.override.yaml" <<'YAML'
services:
  web:
    environment:
      FROM_OVERRIDE: "yes"
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
  local method=$1 path=$2 body=${3:-'{}'}
  curl -s --noproxy '*' -X "$method" -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$PORT$path"
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

# --- new behaviour ---------------------------------------------------------

check "per-container ports are reported" "3000->80" \
  "$(echo "$AFTER" | jq -r '.state.containers[]|select(.service=="web")|.ports[]|"\(.host)->\(.container)"' | head -1)"
check "duplicate ipv4/ipv6 publishers are collapsed" "1" \
  "$(echo "$AFTER" | jq -r '.state.containers[]|select(.service=="web")|.ports|length')"
check "container carries an open URL" "http://localhost:3000" \
  "$(echo "$AFTER" | jq -r '.state.containers[]|select(.service=="web")|.url')"
check "stopped containers get no URL" "" \
  "$(api GET "/api/projects/$BLOG" | jq -r '[.state.containers[]|select(.url != "")]|length' | sed 's/^0$//')"

# A variable-declared port is movable by setting that variable; a hard-coded
# one needs the generated !override file.
check "variable port is movable via the variable" "compose-var" \
  "$(api GET "/api/projects/$SHOP" | jq -r .portControl.mechanism)"
check "compose variable name is recorded" "WEB_PORT" \
  "$(api GET "/api/projects/$SHOP" | jq -r '.ports[]|select(.port==3000)|.variable')"
check "hard-coded port falls back to an override file" "compose-override" \
  "$(api GET "/api/projects/$BLOG" | jq -r .portControl.mechanism)"

# Compose would load the override sibling itself if we did not pass -f; since
# we always do, it has to be supplied explicitly or the project silently builds
# and runs a different definition than `docker compose up` would.
# The base directory is stated rather than inferred, so a generated override
# from the cache directory cannot shift it on any Compose version.
if grep -q -- "--project-directory $WORK/projects/shop" "$WORK/state/calls.log"; then
  pass "the project directory is pinned explicitly"
else
  fail "the project directory is pinned explicitly" \
    "not seen in: $(grep -m1 ' up ' "$WORK/state/calls.log")"
fi

if grep -q 'compose.override.yaml' "$WORK/state/calls.log"; then
  pass "the project's own override file is passed through"
else
  fail "the project's own override file is passed through" \
    "compose.override.yaml never appeared in: $(grep -m1 ' up ' "$WORK/state/calls.log")"
fi

# Never stop a service the user did not ask about.
if grep -q -- '--remove-orphans' "$WORK/state/calls.log"; then
  fail "no implicit orphan removal" "compose was invoked with --remove-orphans"
else
  pass "no implicit orphan removal"
fi

echo "  -- moving shop to port 3100 --"
api POST "/api/projects/$SHOP/restart" '{"ports":{"3000":3100}}' > /dev/null
sleep 2
if grep -q 'WEB_PORT=3100' "$WORK/state/calls.log"; then
  pass "port override passed as the compose variable"
else
  fail "port override passed as the compose variable" \
    "WEB_PORT=3100 not seen in: $(tail -3 "$WORK/state/calls.log")"
fi
MOVED=$(api GET "/api/projects/$SHOP")
check "moved port is reported"      "3100" "$(echo "$MOVED" | jq -r '.state.ports[]|select(.declared==3000)|.port')"
check "declared port is remembered" "3000" "$(echo "$MOVED" | jq -r '.state.ports[]|select(.port==3100)|.declared')"
check "container URL follows the move" "http://localhost:3100" \
  "$(echo "$MOVED" | jq -r '.state.containers[]|select(.service=="web")|.url')"

api POST "/api/projects/$SHOP/restart" '{"clearOverrides":true}' > /dev/null
sleep 2
check "reset returns to the declared port" "3000" \
  "$(api GET "/api/projects/$SHOP" | jq -r '.state.ports[]|select(.service=="web")|.port')"

# --- units -----------------------------------------------------------------

BEFORE=$(api GET "/api/projects/$BLOG")
check "units listed before a project is ever started" "web" \
  "$(echo "$BEFORE" | jq -r '.state.units[].name' | tr '\n' ',' | sed 's/,$//')"
check "never-started unit reports its state" "not-created" \
  "$(echo "$BEFORE" | jq -r '.state.units[0].state')"
check "never-started unit shows its configured port" "3999" \
  "$(echo "$BEFORE" | jq -r '.state.units[0].ports[0].configured')"
check "never-started unit has no actual port" "0" \
  "$(echo "$BEFORE" | jq -r '.state.units[0].ports[0].actual // 0')"

UNITS=$(api GET "/api/projects/$SHOP")
check "a portless service still gets a row" "worker" \
  "$(echo "$UNITS" | jq -r '.state.units[]|select(.name=="worker")|.name')"
check "portless row has no ports" "0" \
  "$(echo "$UNITS" | jq -r '.state.units[]|select(.name=="worker")|.ports|length')"
check "running unit reports configured and actual" "3000/3000" \
  "$(echo "$UNITS" | jq -r '.state.units[]|select(.name=="web")|.ports[0]|"\(.configured)/\(.actual)"')"
check "running unit carries an open URL" "http://localhost:3000" \
  "$(echo "$UNITS" | jq -r '.state.units[]|select(.name=="web")|.url')"

echo "  -- killing one service --"
api POST "/api/projects/$SHOP/units/web/kill" > /dev/null
sleep 2
AFTER_KILL=$(api GET "/api/projects/$SHOP")
check "killed unit is stopped"          "exited"  "$(echo "$AFTER_KILL" | jq -r '.state.units[]|select(.name=="web")|.state')"
check "its sibling keeps running"       "running" "$(echo "$AFTER_KILL" | jq -r '.state.units[]|select(.name=="db")|.state')"
check "kill targets one service only"   "1" \
  "$(grep -c 'ARGS: compose .* stop --timeout 10 web' "$WORK/state/calls.log")"
if grep -qE 'ARGS: compose .* (down|stop [^-])' "$WORK/state/calls.log" | grep -v ' web'; then
  fail "kill does not touch the project" "a project-wide command was issued"
else
  pass "kill does not touch the project"
fi

echo "  -- starting that one service back up --"
api POST "/api/projects/$SHOP/units/web/start" > /dev/null
sleep 2
check "restarted unit is running" "running" \
  "$(api GET "/api/projects/$SHOP" | jq -r '.state.units[]|select(.name=="web")|.state')"
check "start targets one service with --no-deps" "1" \
  "$(grep -c 'ARGS: compose .* up --detach --no-deps web' "$WORK/state/calls.log")"

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
