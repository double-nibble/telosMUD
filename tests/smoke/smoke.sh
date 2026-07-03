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
#   4. The CROSS-SHARD handoff reconnect works end to end: a fresh character walks
#      midgaard (shard-a) -> darkwood (shard-b) across the real cross-shard exit, quits,
#      and RECONNECTS into the exact quit room with NO "mid-transfer" rejection — the
#      regression guard for the live 2-shard reconnect bug (aa64b06), at the layer
#      (real Redis directory + two real shards + the gate's routing) where it surfaced.
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
#
# This is the FULL long-running set: omitting a service means a crash-looping one
# passes smoke silently. In particular world-crypt (shard-c), account, and gate-auth
# were previously unlisted — a broken third shard or a down account/second-gate hop
# was invisible to a smoke run. (otel-collector is a best-effort observability sidecar
# with no healthcheck and is intentionally not gated on.)
SERVICES=(postgres redis nats world world-darkwood world-crypt account gate gate-auth)

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

# scripted_session drives one scripted player session against the gate and prints
# everything the gate sent. It is the reusable telnet driver: pass the per-line
# inter-command pause (seconds) as $1, then each subsequent argument is one line to
# send (the trailing \r\n is added). A login name is just the first line; `look`,
# directions, `quit`, etc. follow. The pause lets each gate prompt/render land before
# the next line — generous enough to cover the cross-shard handoff (the market->grove
# move re-dials shard-b before the activation `look` returns).
#
# It prefers `nc` (a single bidirectional pipe); if nc is absent it falls back to a
# bash /dev/tcp coproc. Every session is bounded: `quit` makes the gate close the
# socket (nc returns on its own), and `nc -w` caps an idle session as a backstop so a
# wedged stack can never hang CI.
scripted_session() {
  local pause="$1"; shift
  local idle=$(( pause * (${#} + 4) + 10 ))   # generous idle cap: scales with line count
  if command -v nc >/dev/null 2>&1; then
    _feed_lines "$pause" "$@" | nc -w "$idle" "${GATE_HOST}" "${GATE_PORT}"
  else
    exec {gate}<>"/dev/tcp/${GATE_HOST}/${GATE_PORT}" || return 1
    cat <&${gate} &
    local reader=$!
    _feed_lines "$pause" "$@" >&${gate}
    exec {gate}>&-
    sleep 1
    kill "$reader" 2>/dev/null || true
    wait "$reader" 2>/dev/null || true
  fi
}

# _feed_lines emits one input line at a time on stdout, each preceded by `pause`
# seconds so the gate has time to render the prior response. A leading pause lets the
# "By what name" prompt arrive before the name is sent.
_feed_lines() {
  local pause="$1"; shift
  sleep "$pause"
  local line
  for line in "$@"; do
    printf '%s\r\n' "$line"
    sleep "$pause"
  done
}

# telnet_session is the original single-look session (login -> look -> quit), kept as a
# thin wrapper over scripted_session so the look smoke below reads the same as before.
telnet_session() {
  scripted_session 2 'Smoketest' 'look' 'quit'
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

# assert_cross_shard_reconnect is the END-TO-END regression guard for the cross-shard
# handoff reconnect bug (fixed in aa64b06). It reproduces, against the REAL 2-shard
# docker stack, the exact journey a user hit on `make up`:
#
#   Session 1: log in a FRESH character -> walk the cross-shard route
#       temple --n--> market --n--> grove (CROSS-SHARD: shard-a/midgaard handoff to
#       shard-b/darkwood) --n--> hollow -> quit.
#   Session 2: reconnect the SAME character.
#
# The regression: pre-fix, the handoff snapshot dropped the PersistID and the relog
# routed to the stale home shard, so the frozen source orphan rejected the reconnect
# with "character is mid-transfer" and the player could not log back in. This asserts
# the reconnect SUCCEEDS, renders the exact quit room ("A Dark Hollow" on shard-b), and
# that "mid-transfer" NEVER appears. The in-process bufconn+miniredis guard
# (TestCrossShardHandoffPersistsAndReconnects) only approximates this; THIS exercises
# real Redis directory placement + two real world shards + the gate's real routing —
# the layer at which the bug actually surfaced.
#
# Each character name is unique per invocation so a re-run (or the --twice second pass)
# never collides with a row already persisted in the volume.
assert_cross_shard_reconnect() {
  # Name must satisfy the gate's validateName: <=20 runes, no leading digit, no dot.
  # Keep it short and unique-per-invocation so a re-run / the --twice second pass never
  # collides with a row already in the volume. PID%10000 + run keeps us well under 20.
  local tag="$(( $$ % 10000 ))r${run:-1}"
  local char="Strav${tag}"
  local deadline=$(( $(date +%s) + 90 )) out1="" out2=""
  # PAUSE per command: generous, because the cross-shard move re-dials shard-b and the
  # activation `look` must land before the next `north`. Bounded by scripted_session's
  # idle cap, never open-ended.
  local pause=3
  while :; do
    # --- Session 1: fresh char walks across the shard boundary, then quits.
    out1="$(scripted_session "$pause" "$char" 'north' 'north' 'north' 'quit' 2>/dev/null || true)"
    # Did the cross-shard walk land us in darkwood's hollow? (Proves the handoff worked
    # and we're live on shard-b.) If not yet — likely shards not all registered — retry.
    if ! printf '%s' "$out1" | grep -qi 'Dark Hollow'; then
      if (( $(date +%s) >= deadline )); then
        printf '%s' "$out1" | grep -qi 'By what name' \
          || fail "cross-shard walk: gate never presented the login prompt; got: $(printf '%s' "$out1" | head -c 400)"
        fail "cross-shard walk never reached 'A Dark Hollow' (handoff to shard-b failed?); got: $(printf '%s' "$out1" | head -c 600)"
      fi
      sleep 3
      continue
    fi

    # The quit flush + directory CAS are async; give the destination a beat to persist
    # the new location before reconnecting. Bounded, not open-ended.
    sleep 3

    # --- Session 2: reconnect the SAME character. Must NOT be mid-transfer; must land
    # back in the exact quit room (hollow, on shard-b) — the heart of the regression.
    out2="$(scripted_session "$pause" "$char" 'look' 'quit' 2>/dev/null || true)"

    if printf '%s' "$out2" | grep -qi 'mid-transfer'; then
      fail "RECONNECT REJECTED 'mid-transfer' (cross-shard reconnect regression!); session2 output: $(printf '%s' "$out2" | head -c 600)"
    fi
    if printf '%s' "$out2" | grep -qi 'Dark Hollow'; then
      log "cross-shard reconnect OK: $char walked midgaard->darkwood, quit in hollow, reconnected into hollow (no mid-transfer)"
      return 0
    fi

    # Reconnect produced neither mid-transfer nor the expected room. If we still have
    # budget it may be a slow flush/registration; retry the whole journey with a fresh
    # name. Otherwise fail loudly with both captures.
    if (( $(date +%s) >= deadline )); then
      fail "reconnect did not render 'A Dark Hollow' (and was not 'mid-transfer'); session2 output: $(printf '%s' "$out2" | head -c 600)"
    fi
    # Fresh short name for the retry (still <=20 runes, no leading digit): suffix with
    # seconds-mod so a half-completed prior attempt's row never interferes.
    char="Strav${tag}t$(( $(date +%s) % 100000 ))"
    sleep 3
  done
}

# assert_gate_auth_reachable connects to the SECOND gate (gate-auth, :4001) and asserts it
# presents the account-backed OAuth sign-in link ("To sign in, open this link ..."), NOT the
# :4000 dev-autoauth bare-name prompt. This is a wiring check across a surface the :4000-only
# look/reconnect asserts never touch: gate-auth must be UP and must reach the account service
# (which mints the device-auth link). A crash-looping gate-auth, a broken gate->account hop, or
# a gate-auth accidentally started in autoauth mode all fail here.
assert_gate_auth_reachable() {
  local port="${GATE_AUTH_PORT:-4001}"
  local deadline=$(( $(date +%s) + 60 )) out=""
  while :; do
    if command -v nc >/dev/null 2>&1; then
      out="$(printf '' | nc -w 6 "${GATE_HOST}" "${port}" 2>/dev/null || true)"
    else
      # /dev/tcp fallback: open, read for a few seconds, then close (no input to send).
      out="$( { exec {g}<>"/dev/tcp/${GATE_HOST}/${port}" && { cat <&${g} & r=$!; sleep 5; kill "$r" 2>/dev/null; wait "$r" 2>/dev/null; }; } 2>/dev/null || true)"
    fi
    if printf '%s' "$out" | grep -qi 'To sign in, open this link'; then
      if printf '%s' "$out" | grep -qi 'By what name'; then
        fail "gate-auth :$port showed the dev-autoauth bare-name prompt, not the account-backed sign-in link (started in the wrong mode?)"
      fi
      log "gate-auth :$port reachable + account-backed (OAuth sign-in link presented)"
      return 0
    fi
    if (( $(date +%s) >= deadline )); then
      fail "gate-auth :$port did not present the account-backed sign-in link within deadline (gate-auth down or account hop broken?); got: $(printf '%s' "$out" | head -c 300)"
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
  assert_gate_auth_reachable
  assert_cross_shard_reconnect
  if (( run < RUNS )); then
    # Stop the long-running services but KEEP the volume so run 2 re-seeds a populated DB.
    log "stopping services (keeping the Postgres volume for the re-seed run)"
    $COMPOSE down >/dev/null 2>&1 || true
  fi
done

log "SMOKE PASS"
