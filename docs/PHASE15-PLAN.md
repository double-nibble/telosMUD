# Phase 15 — Terminal-native OAuth (login rework)

Status: **COMPLETE (2026-06-30).** 15.1 → 15.6 all landed CI-green. Reworked Phase 14's front-end: the website-
centric link-code/passphrase/SSH login is replaced by a single **terminal-native OAuth device flow**. The
former "Phase 15 — Hardening & scale" shifted to **Phase 16**.

**Landed:** 15.1 device-auth backend (Redis device sessions + StartDeviceAuth/PollDeviceAuth) · 15.2 the
one-click broker (`internal/web` stripped to `/login/{device_code}` + the OAuth callback) · 15.3 gate OAuth
device login (replaces the code/passphrase prompt; the dead login paths removed) · 15.4 prompt-driven char
select + create (GetChargenFlow/CreateChargenCharacter, char cap; the at-cap fix keeps a full account on the
selection menu) · 15.5a SSH transport removed · 15.5b passphrase + link codes + the dead RPCs/packages removed
(OAuth-only) · 15.6 the `TELOS_DEV_AUTOAUTH` bypass + the primary `:4000` gate flipped account-backed (dev sets
the bypass so smoke/e2e stay headless) + the success page auto-closes. The flow is proven by the gate journey
suite (device login, prompt-chargen create, dev-autoauth, at-capacity) + the gated account→world chargen
journey + the hermetic world build-survives-reload test; the live TLS + browser-OAuth + reconnect capstone is
exercised manually on `:4001` + the `:8080` broker.

## Goal

`connect localhost:4000` → the gate shows a one-click link → the browser does OAuth → the telnet session is
authed → pick or create a character (prompt-driven chargen) → play. **No passwords, ever; auth fully
externalized to the OAuth provider.**

## Decided (user, 2026-06-30)

- **One-click broker** (not the native RFC-8628 device flow): the terminal shows a clickable link to OUR tiny
  auth endpoint, which 302-redirects into the provider's OAuth; the callback marks the waiting session authed.
  No code to type. The web surface shrinks to a bare **auth bridge** — no dashboard, no chargen form, no Play.
- **OAuth-only.** Remove passphrase auth (14.5) AND SSH pubkey auth + the SSH transport (14.6b) entirely. No
  key-based auth. Keep **plain telnet + TLS telnet (`telnets://`)** as the only transports.
- **Prompt-driven onboarding.** Character select + chargen happen as telnet prompts (the content-driven chargen
  ENGINE is reused unchanged — only the renderer moves from web form to prompts).

## Architecture — the brokered device flow

The Redis link-code store (14.2) is repurposed as the **device session** (the inverse of 14.2: the telnet
side now MINTS the pending code, the browser side FULFILLS it).

1. **connect** → gate calls account `StartDeviceAuth(connInfo)` → a random `device_code` (Redis, ~10 min TTL,
   status=pending) + a `verification_uri` (`http://<broker>/login/<device_code>`).
2. Gate prints the link (OSC-8 hyperlink + a plaintext fallback) and **polls** account `PollDeviceAuth(device_code)`
   (a few-second interval, until authed / expired / the player disconnects).
3. Player opens the link → broker `/login/{device_code}`: validate pending → stash the device_code in a signed
   flow cookie → start OAuth (PKCE) → 302 to the provider.
4. Provider → broker `/auth/<provider>/callback`: exchange code → fetch identity → resolve-or-create account →
   **mark the device session authed** (device_code → {authed, account_id}) → render "✓ Logged in — return to
   your terminal."
5. Gate's poll sees `authed` → gets `account_id` (+ characters) → issues the signed session assertion → enters
   **character select**.
6. **Character select / chargen (prompts):** list the account's characters numbered (pick one) or `new` (up to a
   configurable cap); no characters → must create. `new` walks the content chargen flow's steps as prompts
   (reusing `content.ValidateChargen` + the account `BuildCharacter`), then spawns — the world applies the
   build on first spawn (Phase 14.8a, unchanged).

## Reused vs removed

**Reused:** account/character store, `oauth.go` (PKCE/exchange/identity), signed-cookie helpers (for the broker's
OAuth state), signed session assertions, the single-session lock, the **entire chargen engine** (`chargen_defs`,
`ValidateChargen`, `BuildCharacter`, world first-spawn apply), TLS telnet transport.

**Removed:** the website dashboard + chargen form + Play page + the website-minted-link-code direction; passphrase
auth (store methods + gate path + `account_auth` usage); the SSH transport + pubkey auth + `ResolveSSHKey` +
`ssh_keys`; the name-only stub login as a *prod* path (see 15.6 for the dev/test seam).

## Slice breakdown

- **15.1 — Device-auth backend.** Account gRPC `StartDeviceAuth` / `PollDeviceAuth` over a Redis device-session
  store (repurpose `linkcodes`). The proto grows the two RPCs. Hermetic + gated tests.
- **15.2 — The one-click broker.** Strip `internal/web` to `/login/{device_code}` + `/auth/<provider>/callback`
  + a confirmation page; reuse oauth.go + session signing; the callback marks the device session authed. Delete
  the dashboard/form/play handlers + templates. Hermetic flow test (stub provider).
- **15.3 — Gate OAuth login.** Replace the gate's code/passphrase prompt with: `StartDeviceAuth` → print the
  OSC-8 link (+ plaintext) → poll → authed. The primary gate (:4000) is OAuth-only.
- **15.4 — Prompt-driven char select + chargen.** List/pick/new (configurable cap); walk the content chargen
  flow as prompts; reuse `BuildCharacter`. The world applies the build on first spawn (unchanged).
- **15.5 — Transport + dead-code cleanup.** Keep plain + TLS telnet; REMOVE the SSH transport/pubkey path and
  the passphrase path (gate + account + store + the `account_auth`/`ssh_keys` migrations stay as historical
  migrations but the code paths go). Update docs/ACCOUNT.md.
- **15.6 — Test migration + capstone.** A **dev/test auth seam** (`TELOS_DEV_AUTOAUTH=1`, default OFF) lets the
  gate auto-auth `connect <name>` to a seeded account WITHOUT a browser — so smoke/e2e stay green headlessly;
  prod stays OAuth-only. e2e additionally drives the real broker (stub provider). Capstone milestone: connect
  over TLS → click link → OAuth → authed → create a character via prompts → play → reconnect (survives restart).

## Open design points (recommendations — confirm or adjust at approval)

1. **Phase number / roadmap.** Call this **Phase 15**; the old "Hardening & scale" becomes **Phase 16**. (Update
   ROADMAP.md.) — *Recommend yes.*
2. **Test/dev auth seam.** A config-gated `TELOS_DEV_AUTOAUTH` that restores a name-only auto-auth for headless
   smoke/e2e (insecure, dev-only, default OFF). The alternative — driving the full browser OAuth in smoke — isn't
   feasible against real GitHub in CI. — *Recommend the dev seam.*
3. **Character cap.** Default **3** per account, `TELOS_MAX_CHARACTERS` configurable. — *Recommend 3.*
4. **Provider scope.** GitHub first (already configured); the broker stays provider-generic so Google/Discord
   are a config add later. — *Recommend GitHub-first.*
5. **Broker still served by telos-account** (in-process, as today) on `WEB_LISTEN`; it's now an auth bridge, not
   a site. — *Recommend keep in telos-account.*
