# Phase 14 — Auth & website

The close of **Track E's first phase**: replace the credential-less stub login (`gate.go` — "By what name
shall you be known?", `account_id` nullable since Phase 1) with real accounts, OAuth, encrypted transports,
and a content-driven chargen website. Design: [ACCOUNT.md](ACCOUNT.md). Owned by a new `telos-account`
service; consumed by `telos-gate`; the world trusts it via signed assertions, never a hot-path RPC.

Status: **LOCKED (2026-06-30).** Building 14.1 → 14.8 + capstone. All four sub-forks (D-A server-rendered Go
templates + htmx · D-B Ed25519 assertions · D-C GitHub OAuth first · D-D OWASP Argon2id) confirmed. OAuth
provider credentials are configured by the user at 14.7 (a GitHub OAuth app — client id/secret + redirect
URI); 14.1–14.6 build + CI-verify against stubs with no external setup.

---

## Settled forks (from review)

1. **FULL website** — OAuth sign-in + account dashboard + character management + a polished content-driven
   chargen UI (not a minimal bridge). This makes the web layer 2 slices (14.7 core + 14.8 chargen UI).
2. **Transports: TLS + SSH default-ON; plain telnet OPT-IN.** The gate listens on TLS telnet + SSH by
   default; **unencrypted plain telnet is disabled unless explicitly enabled** at gate startup (a
   `--allow-plaintext` flag / `TELOS_GATE_ALLOW_PLAINTEXT=1`). Credentials never cross an unencrypted wire
   by default.
3. **Single-session conflict: TAKEOVER** — a new login drops the old connection ("you have been
   disconnected by a new login"), the standard MUD UX.

## Open sub-forks (recommendations — confirm or override at lock)

- **D-A — Frontend stack.** *Recommend:* `telos-account` serves **server-rendered Go `html/template` +
  htmx** for the interactive chargen — in-repo, no separate build pipeline, server-testable end to end.
  Alternative: a separate SPA (heavier; its own toolchain). 
- **D-B — Assertion signing.** *Recommend:* **Ed25519** (small, fast, modern) for the session assertion;
  account publishes its public key (a `/.well-known`-style endpoint or a Redis-cached key the shard reads),
  rotated periodically with a key-id in the assertion header.
- **D-C — OAuth provider first.** *Recommend:* wire **GitHub first** (the repo lives there; easiest to
  test), with Google/Discord as config-parallel providers added behind the same identity flow.
- **D-D — Argon2id parameters.** *Recommend:* the OWASP baseline (m=19 MiB, t=2, p=1), tunable via config.

---

## Slice breakdown

### 14.1 — `telos-account` service skeleton + schema + gate seam
The spine everything hangs off. New `cmd/telos-account` (gRPC server) + its internal API surface, the schema,
and the gate's account-client seam (an interface + a test stub, so 14.2+ can be tested before the real
service exists).
- Schema (migration): extend `accounts` (the stub from 00002 — fix the stale "Phase 13" comment) +
  `account_identities` (provider, provider_uid UNIQUE); add `account_auth` (Argon2id passphrase) + `ssh_keys`
  (fingerprint PK). (PERSISTENCE.md §2, ACCOUNT.md §11.)
- gRPC API: `Authenticate`, `RedeemLinkCode`, `VerifyPassphrase`, `ResolveSSHKey`, `ListCharacters`,
  `CreateCharacter`, `ReserveName`. The proto + a `buf generate` pass.
- Gate seam: an `AccountClient` interface in the gate; a `stubAccount` for hermetic tests; the real gRPC
  client behind a flag.
- **Done when:** `telos-account` boots, the schema migrates, and the gate can call a stubbed `ListCharacters`
  over the seam (no real auth yet).

### 14.2 — Link codes (the primary telnet bridge)
The credential-less happy path. Account mints a single-use, ~40-bit, ~5-min-TTL code in Redis keyed to
`(account_id[, character_id])`; `connect <code>` at the gate redeems it.
- `MintLinkCode` (account, for the website's "Play" button) + `RedeemLinkCode` (gate → account: atomic
  consume, returns account + character list + the session assertion stub).
- Gate login flow: replace the bare name prompt with a menu — `connect <code>` → character select/enter world.
- **Done when:** a (test-minted) link code redeems once, returns the character list, enters the world; a
  second redeem of the same code fails.

### 14.3 — Signed session assertions (gate↔world trust)
Decouple the world from account at connect time. Account issues a short-lived signed
`{account_id, character_id, session_id, exp}` (D-B: Ed25519); the gate carries it in the `Attach` frame;
shards verify against account's published public key (cached, offline — no per-connect RPC).
- Assertion mint (account) + verify (world); key publication + rotation (key-id + cached public key).
- Wire it into `Attach` (PROTOCOL.md §1) beside the existing handoff token; the world rejects an
  unverifiable/expired assertion. Distinct from the intra-cluster handoff token.
- **Done when:** a forged/expired assertion is refused by the shard; a valid one enters the world with the
  asserted identity, verified with no call to account.

### 14.4 — Single-session lock (takeover)
One live session per character. A Redis per-character lock with a TTL, heartbeated by the session; a crashed
connection's lock self-expires. **Takeover policy:** a second login drops the old connection.
- Acquire-on-enter / release-on-exit / heartbeat; the takeover signal to the old gate session ("disconnected
  by a new login"). Complements the epoch/state_version single-WRITER guard (this prevents two CONNECTIONS).
- **Done when:** two logins for one character → the first is cleanly dropped, the second plays; a crash
  releases the lock within the TTL.

### 14.5 — Passphrase auth + rate limiting
The website-less convenience path (discouraged on plain telnet). Argon2id (D-D) in `account_auth`;
`connect <name> <passphrase>`; per-account + per-IP rate limits with lockout/backoff; a cleartext warning
when sent over an unencrypted transport.
- `SetPassphrase` (web/account) + `VerifyPassphrase` (gate→account) with the rate-limit/lockout state.
- **Done when:** a correct passphrase logs in; repeated failures lock out with backoff; a plain-telnet
  passphrase entry emits the cleartext warning.

### 14.6 — Encrypted transports (TLS + SSH; plain opt-in)
The transport posture (settled fork #2). Gate listens on TLS telnet + an SSH server by default; plain telnet
only when explicitly enabled.
- TLS telnet (TELNETS) listener; cert config. SSH server (pubkey auth → `ResolveSSHKey` → account); unknown
  key falls back to interactive link-code entry (+ optional key registration).
- Plain telnet gated behind `--allow-plaintext` / `TELOS_GATE_ALLOW_PLAINTEXT`; OFF by default. GMCP/MCCP
  negotiation identical across transports.
- **Done when:** a client connects over TLS and over SSH (pubkey); plain telnet is refused unless the flag is
  set; the same GMCP/MCCP handshake runs on all three.

### 14.7 — OAuth + account website core ✅ DONE (2026-06-30)
The browser front door (D-A: server-rendered Go templates + htmx; D-C: GitHub first). OAuth 2.0 / OIDC
Authorization Code + PKCE: provider sign-in → `account_identities` lookup (found→login, not-found→create) →
session cookie. Account dashboard + character management; the **Play** button mints a link code (→ 14.2).
- PKCE + `state` CSRF + strict redirect-URI allowlist; account linking only while authenticated (never
  auto-merge by email). Dashboard: characters, linked providers, passphrase set, SSH keys.
- **Done when:** sign in with GitHub on the website, land on a dashboard, click Play → get a link code that
  redeems at the gate.
- **Landed:** `internal/web` (oauth.go PKCE+exchange+identity, session.go signed cookies, server.go routes,
  templates.go pages); store `FindIdentity`/`CreateAccountWithIdentity`/`AccountDisplayName`; config
  `Web*`/`Github*`/`OAuthRedirectURL` + env; wired into `cmd/telos-account` (serves :8080 when `WEB_LISTEN`
  set); `account` service in docker-compose (`env_file: auth.local.env` required:false; GITHUB creds gitignored).
  Hermetic flow test stubs the provider via httptest; gated PG round-trip for the identity methods.
  **Deferred to the capstone:** pointing the gate at telos-account (`ACCOUNT_TARGET`) so a web-minted code
  redeems end-to-end over telnet — held back here so stub-login smoke/e2e stays green.

### 14.8 — Content-driven chargen front-end ✅ DONE (2026-06-30)
<!-- 14.8a (chargen_defs + world first-spawn apply) + 14.8b (validator + account BuildCharacter + web form) all landed. -->

Chargen is content, not hardcoded (PRINCIPLES.md), and — per the user (2026-06-30) — **not boxed into one
system**: content drives *how* generation works (roll-and-assign, point-buy, standard array, 1-stat-then-
spend-XP, …). The abstraction is a **content `chargen` flow = ordered STEPS**, each a `kind`:
- `bundle_choice` — pick N bundles of a `bundle_kind` (race, class, background) → resolves to the chosen refs.
- `point_buy` — allocate a `points` budget across `attributes` under a per-target `cost` curve + min/max →
  resolves to the chosen attribute values. (Implemented now.)
- (future kinds — `array_assign`, `roll` — are a content write + a small step-kind handler, not a rewrite.)

Storage/flow:
- **`chargen_defs`** — the 8th def-table (ref+pack+JSONB body, the full def-table precedent: DTO + loader +
  store read/write/strip/migration + gated round-trip + world/registry). One flow per pack by convention.
- **Apply on FIRST SPAWN (Model A).** Chargen's OUTPUT (chosen bundle refs + chosen attribute values) is
  recorded into the new character's INITIAL STATE as a pending-chargen marker. `CreateAccountCharacter`
  already takes the `state []byte`. The **world**, on first spawn of a brand-new character, reads the marker,
  SETS the point-buy attribute bases, then runs the existing `apply_bundle` grant path for each chosen bundle
  (single-writer, authoritative), clears the marker, and persists (restart-safe). No grant interpreter in
  telos-account.
- **telos-account** only READS content rows (the pack's `chargen` flow + `bundle_defs`, from Postgres) to
  render the form + VALIDATE the submission server-side (bundle kinds match, point-buy within budget/bounds),
  then writes the marker. Adding a race is a content write — the form updates with no code change.

Sub-slices: **14.8a** content schema (`chargen_defs`) + loader + store + migration + world first-spawn
application + account-side validation/create; **14.8b** the web chargen flow (read flow → render steps →
POST validate + create).
- **Done when:** create a character on the web from the demo class+race + a point-buy allocation; on first
  connect the bundle grants + bought attributes are applied (and survive a restart); a newly-content-added
  race appears in the form with no code change.

### 14.x — Capstone (the done-when)
The full front door, end to end: **create an account on the web (GitHub OAuth), build a character from
content-driven chargen, get a link code, `connect` over TLS (or SSH), enter the world** — the session
assertion verifies offline at the shard, the single-session lock holds (a second login takes over), and the
account+character survive a restart. Tests (hermetic stubs for OAuth/providers) + the milestone.

---

## Notes / dependencies
- Cashes in two long-deferred threads: the **chargen front-end** (deferred from Phase 11) and the **auth
  bridge** (stubbed since Phase 1).
- The `auth-engineer` agent owns this domain; `security-auditor` should review 14.2/14.3/14.5/14.6/14.7
  (link codes, assertions, passphrases, transports, OAuth) — the highest-value attack surface in the project.
- After Phase 14: **Phase 15** (hardening & scale) + the end-of-roadmap **GitHub wiki** push.
