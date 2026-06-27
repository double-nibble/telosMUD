#!/usr/bin/env bash
# smoke.sh — the cheapest end-to-end catch for the seed-class bug (and "does the
# stack even come up").
#
# It brings up the FULL docker stack (postgres, redis, nats, migrate, seed, two
# world shards, gate), then asserts the three things a hermetic unit test
# structurally cannot see:
#
#   1. The one-shot seed container EXITED 0 — the deletePack/idempotency regression
#      (a populated Postgres volume + a second `make up`) shows up here and nowhere
#      in `go test ./...`.
#   2. Every long-running service is HEALTHY (or at least running) — the stack wires
#      up, no container crash-loops.
#   3. A telnet client can CONNECT, log in, and `look` — the player-facing path is
#      live end to end (gate -> world -> directory).
#
# By default it runs the stack ONCE. Pass --twice (or SMOKE_TWICE=1) to run the
# whole bring-up TWICE against the SAME persisted Postgres volume — that is the
# real seed-bug reproduction (the second seed re-imports into a populated DB and
# must strip-and-replace, not collide on a duplicate key). CI runs --twice.
#
# Exit 0 = smoke passed. Any non-zero = a defined failure with a printed reason.
# The stack is torn down on exit (success or failure) unless SMOKE_KEEP=1.
set -euo pipefail

COMPOSE="${COMPOSE:-docker compose -f deploy/docker-compose.yml}"
GATE_HOST="${GATE_HOST:-localhost}"
GATE_PORT="${GATE_PORT:-4000}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-180}"   # seconds to wait for healthy + seed-exit
RUNS=1
if [[ "${1:-}" == "--twice" || "${SMOKE_TWICE:-0}" == "1" ]]; then
  RUNS=2
fi

# Long-running services that must end up healthy/running. The one-shots
# (migrate, seed) are checked separately by exit code.
SERVICES=(postgres redis nats world world-darkwood gate)

log()  { printf '\n=== %s\n' "$*"; }
fail() { printf '\nSMOKE FAIL: %s\n' "$*" >&2; dump_logs; exit 1; }

dump_logs() {
  printf '\n--- docker compose ps ---\n' >&2
  $COMPOSE ps >&2 || true
  printf '\n--- seed logs ---\n' >&2
  $COMPOSE logs seed >&2 2>/dev/null || true
}

cleanup() {
  if [[ "${SMOKE_KEEP:-0}" == "1" ]]; then
    log "SMOKE_KEEP=1: leaving the stack up"
    return
  fi
  log "tearing down the stack"
  $COMPOSE down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

# container_state SERVICE -> prints the container's health (or "running"/"exited:N").
container_state() {
  local svc="$1" cid
  cid="$($COMPOSE ps -q "$svc" 2>/dev/null || true)"
  if [[ -z "$cid" ]]; then
    echo "missing"
    return
  fi
  local status health
  status="$(docker inspect -f '{{.State.Status}}' "$cid" 2>/dev/null || echo unknown)"
  health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid" 2>/dev/null || echo none)"
  if [[ "$status" == "exited" ]]; then
    echo "exited:$(docker inspect -f '{{.State.ExitCode}}' "$cid" 2>/dev/null || echo '?')"
  elif [[ "$health" != "none" ]]; then
    echo "$health"   # healthy | unhealthy | starting
  else
    echo "$status"   # running
  fi
}

# wait_healthy blocks until every SERVICE is healthy/running, failing fast on a
# crash (exited non-zero) or the deadline.
wait_healthy() {
  local deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
  while :; do
    local all_ok=1
    for svc in "${SERVICES[@]}"; do
      local st; st="$(container_state "$svc")"
      case "$st" in
        healthy|running) ;;
        exited:0)        ;;  # tolerate a service that legitimately completed
        exited:*)        fail "service '$svc' exited non-zero ($st)" ;;
        *)               all_ok=0 ;;
      esac
    done
    if [[ "$all_ok" == "1" ]]; then return 0; fi
    if (( $(date +%s) >= deadline )); then
      printf '\ncurrent states:\n' >&2
      for svc in "${SERVICES[@]}"; do printf '  %-16s %s\n' "$svc" "$(container_state "$svc")" >&2; done
      fail "timed out after ${HEALTH_TIMEOUT}s waiting for services to be healthy"
    fi
    sleep 3
  done
}

# assert_seed_ok checks the one-shot seed container ended with exit 0. This is the
# seed-bug guard: a duplicate-key collision on a re-import makes seed exit non-zero.
assert_seed_ok() {
  local cid code
  cid="$($COMPOSE ps -a -q seed 2>/dev/null || true)"
  [[ -n "$cid" ]] || fail "seed container not found (did the stack come up?)"
  code="$(docker inspect -f '{{.State.ExitCode}}' "$cid" 2>/dev/null || echo '?')"
  [[ "$code" == "0" ]] || fail "seed container exited $code (expected 0; the re-import/idempotency regression lands here)"
  log "seed exited 0 (content imported / re-imported cleanly)"
}

# telnet_session drives one scripted player session against the gate and prints
# everything the gate sent. It feeds login + look + quit on stdin with pauses so each
# prompt lands before the next line. It prefers `nc` (a single bidirectional pipe);
# if nc is absent it falls back to a bash /dev/tcp coproc.
telnet_session() {
  # The scripted player: pause, send a name, look, quit. `quit` makes the gate close
  # the socket, so nc returns on its own; -w 15 bounds an idle session as a backstop.
  if command -v nc >/dev/null 2>&1; then
    { sleep 1; printf 'Smoketest\r\n'; sleep 2; printf 'look\r\n'; sleep 2; printf 'quit\r\n'; sleep 1; } \
      | nc -w 15 "${GATE_HOST}" "${GATE_PORT}"
  else
    exec {gate}<>"/dev/tcp/${GATE_HOST}/${GATE_PORT}" || return 1
    cat <&${gate} &
    local reader=$!
    { sleep 1; printf 'Smoketest\r\n'; sleep 2; printf 'look\r\n'; sleep 2; printf 'quit\r\n'; sleep 1; } >&${gate}
    exec {gate}>&-
    sleep 1
    kill "$reader" 2>/dev/null || true
    wait "$reader" 2>/dev/null || true
  fi
}

# assert_telnet_look connects to the gate, logs in, and asserts a 'look' renders a
# room (the player-facing path is live end to end: gate -> world -> directory). It
# retries the connect for a bit because the gate accepts sockets before the world
# shards have registered in the directory — an early connect gets "no world
# available", which is a transient startup state, not a failure.
assert_telnet_look() {
  local deadline=$(( $(date +%s) + 60 )) out=""
  while :; do
    out="$(telnet_session 2>/dev/null || true)"
    if printf '%s' "$out" | grep -qiE 'Temple|Square|Exits|exits'; then
      log "telnet connect + look rendered a room"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      printf '%s' "$out" | grep -qi 'By what name' \
        || fail "gate did not present the login prompt; got: $(printf '%s' "$out" | head -c 300)"
      fail "look did not render a room within deadline; got: $(printf '%s' "$out" | head -c 400)"
    fi
    sleep 3
  done
}

for run in $(seq 1 "$RUNS"); do
  if (( RUNS > 1 )); then log "RUN $run of $RUNS (same Postgres volume; run 2 exercises the re-seed)"; fi
  log "make up (build + start full stack)"
  $COMPOSE up -d --build
  wait_healthy
  assert_seed_ok
  assert_telnet_look
  if (( run < RUNS )); then
    # Stop the long-running services but KEEP the volume so run 2 re-seeds a populated DB.
    log "stopping services (keeping the Postgres volume for the re-seed run)"
    $COMPOSE down >/dev/null 2>&1 || true
  fi
done

log "SMOKE PASS"
