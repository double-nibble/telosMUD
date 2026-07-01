# Accounts & authentication

The edge/service concern that bridges browser **OAuth** to the credential-less **telnet**
session. Owned by `telos-account`; consumed by `telos-gate`. Touches the `accounts` /
`account_identities` tables from [PERSISTENCE.md](PERSISTENCE.md) §2.

Authentication is **OAuth-only**: there is no website dashboard, no password/passphrase, and no
SSH transport. A player connects over telnet, is handed a one-click browser link, completes
OAuth, and is dropped into character select — the account service never exposes a credential to
the gate or world.

---

## 1. `telos-account` — responsibilities

- An **OAuth broker** (`internal/web`): a bare auth bridge (no dashboard, no forms) that serves
  the one-click device-login link, runs the OAuth Authorization Code + PKCE flow against the
  provider, resolves-or-creates the account, and marks the pending device session authed.
- An internal **gRPC API** the gate calls: `ListCharacters`, `ReserveName`, `CreateCharacter`,
  `IssueSessionAssertion`, `StartDeviceAuth` / `PollDeviceAuth`, and the chargen RPCs
  (`GetChargenFlow`, `CreateChargenCharacter`).
- Owns: `accounts`, `account_identities`, and character creation (name reservation). It is the
  **only** service that touches OAuth providers; the gate and world never see provider tokens.

## 2. OAuth flow (device / broker)

The telnet side is credential-less: the terminal mints a pending login, the browser fulfills it.
This is the inverse of a link code — the gate shows the URL, the player finishes in a browser.

1. On connect, the gate calls `StartDeviceAuth`, which mints a high-entropy **`device_code`**
   (256-bit, URL-safe, ~10-minute TTL) and returns a one-click link `/login/<device_code>` plus
   a suggested poll interval. The device_code is the bearer capability for that login, so it is
   unguessable and non-replayable.
2. The gate shows the link; the player opens it in a browser.
3. The broker runs standard **OAuth 2.0 / OIDC Authorization Code + PKCE** against the provider
   (**GitHub**): redirect with `state` (CSRF) + PKCE challenge, callback exchanges the code for
   tokens, fetches the profile (`provider_uid`, email).
4. The broker looks up `account_identities(provider, provider_uid)`:
   - **found** → that account.
   - **not found** → create a new `account` + identity.
   Then it marks the device session **authed** with the resolved account.
5. The gate's `PollDeviceAuth` picks up the authed session (one-shot: the session is consumed on
   read), receives the account + character list, and proceeds to character select.

**Account identity by provider, never by email:** email is stored but is **never** an identity
key — provider emails may be unverified or reused, so the engine **never auto-merges by email**
(`internal/store/account.go`).

## 3. Character creation is content-driven

Chargen is **not hardcoded** ([PRINCIPLES.md](PRINCIPLES.md)). The gate walks a
**content-defined chargen flow** as prompts: `GetChargenFlow` returns the steps and selectable
options (races/classes/bundles + point-buy attribute steps), read from content; the player
answers at the terminal. `CreateChargenCharacter` validates the submission server-side (the
point-buy cost curve lives in the service, not on the wire), reserves the unique `name` (CITEXT),
and writes the initial `state` JSONB. Adding a race/class is a content write; the prompts update
with no code change. The current step kinds are `bundle_choice` and `point_buy`.

## 4. Transport encryption posture

The gate listens on two transports; GMCP/MCCP negotiation is identical across them, only the
byte stream's encryption differs.

- **TLS telnet / TELNETS** (the encrypted default) — modern clients (Mudlet, etc.) support it;
  encrypts the whole session.
- **Plain telnet** (legacy clients) — opt-in; the byte stream is unencrypted.

Because auth is OAuth-in-the-browser, no credential ever crosses the telnet wire regardless of
transport — the only thing that transits is the device_code (a one-shot, short-TTL capability).

## 5. Gate ↔ world trust — signed session assertions

When the gate opens the `Play.Connect` stream ([PROTOCOL.md](PROTOCOL.md) §1), the world shard
must trust the asserted `account_id`/`character_id` **without** a synchronous call to account on
the hot path.

- On successful auth, the gate calls `IssueSessionAssertion`; account issues a **short-lived
  signed session assertion** `{account_id, character_id, session_id, exp}`, signed with account's
  private key.
- The gate carries it in the `Attach` frame; shards verify the signature against account's
  **published public key** (cached, rotated periodically) — fully offline, no per-connect RPC.
- This decouples the world from account at connect time and means a compromised gate can't forge
  identities it wasn't granted. Distinct from the intra-cluster handoff token.

## 6. Single-session lock

- On entering the world, the gate acquires a **Redis lock per character** (one live session per
  character, `internal/sessionlock`). The lock has a TTL and is heartbeated by the session, so a
  crashed connection's lock self-expires.
- **Conflict policy — takeover:** a second login for the same character **takes over**; the old
  connection is dropped ("you have been disconnected by a new login").
- This complements the `epoch` / `state_version` single-writer guard (PERSISTENCE.md §7): the
  session lock prevents two *connections*; epoch/version prevent two *shard owners*.

## 7. Development bypass

`TELOS_DEV_AUTOAUTH` enables a dev/test bypass (`WithDevAutoAuth`): an account-backed gate accepts
a bare `name` login instead of the browser OAuth flow, so headless smoke/e2e and local dev work
without a browser. It is **insecure** and gated behind the env flag — never enabled in production.

## 8. Security checklist

- OAuth: PKCE, `state` CSRF guard, strict redirect-URI allowlist, store minimal provider data.
- No email-based account auto-merge — identity is `(provider, provider_uid)` only.
- Device codes: high entropy (256-bit), single-use, short TTL (~10 min).
- Session assertions: short-lived, signed, verified offline by shards.
- Single-session lock with TTL + heartbeat; takeover drops the displaced connection.
- Prefer the encrypted (TLS) transport.
