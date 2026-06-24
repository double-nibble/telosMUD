# Accounts & authentication

The edge/service concern that bridges browser **OAuth** to the credential-less **telnet**
session. Owned by `telos-account` (service + website); consumed by `telos-gate`. Touches the
`accounts` / `account_identities` tables from [PERSISTENCE.md](PERSISTENCE.md) §2.

Status: **proposal** — three choices flagged in §11.

---

## 1. `telos-account` — responsibilities

- The **website**: OAuth sign-in, account dashboard, character management, the "play" bridge.
- An internal **gRPC API** the gate calls (authenticate, redeem code, verify passphrase,
  resolve SSH key, character list).
- Owns: `accounts`, `account_identities`, `account_auth`, `ssh_keys`, and character creation
  (name reservation). It is the **only** service that touches OAuth providers and credentials;
  the gate and world never see provider tokens.

## 2. OAuth flow (web)

Standard OAuth 2.0 / OIDC **Authorization Code + PKCE**:

1. User picks a provider (Google / Discord / GitHub) on the website.
2. Redirect to the provider with `state` (CSRF) + PKCE challenge.
3. Provider redirects back with a code; account exchanges it for tokens and fetches the
   profile (`provider_uid`, email).
4. Look up `account_identities(provider, provider_uid)`:
   - **found** -> log into that account.
   - **not found** -> create a new `account` + identity.
5. A website session cookie is established.

**Account linking:** additional providers can be linked **only while already authenticated**
(from account settings). We **never auto-merge by email** — provider emails may be unverified
or reused, so email is not an identity key; a collision is surfaced, not silently merged.

## 3. Character creation is content-driven

Chargen is **not hardcoded** ([PRINCIPLES.md](PRINCIPLES.md)): the website reads the
`race_defs` / `class_defs` / `attribute_defs` tables to present available choices and starting
allocations. Adding a race is a content write; the signup page updates with no code change.
Creating a character reserves the unique `name` (CITEXT) and writes the initial `state` JSONB.

## 4. The telnet bridge — one-time link codes (primary)

The credential-less path, best UX:

1. Signed into the website, the user clicks **Play** -> account mints a short-lived **link
   code** (≈8 chars, ~40 bits entropy) in Redis, keyed to `(account_id[, character_id])`,
   **single-use**, ~5 min TTL.
2. The user connects via telnet and types `connect <code>` (or enters it at the menu).
3. Gate -> `account.RedeemLinkCode(code, connInfo)` -> account atomically consumes the code and
   returns the account + character list (plus the signed session assertion, §9).
4. Gate proceeds to character selection / enters the world.

One-shot + short TTL + high entropy keeps the cleartext-interception window tiny and
non-replayable.

## 5. MUD passphrase (optional) & the cleartext problem

For logging in without the website, a user may set a **passphrase** (Argon2id, in
`account_auth`); telnet: `connect <name> <passphrase>`, rate-limited with lockout.

But **plain telnet is unencrypted** — a passphrase (and all play) crosses the wire in
cleartext. Mitigations, in order of preference: prefer **link codes** (one-shot), offer
**TLS** and **SSH** (encrypted, §6/§8), and **warn** when a passphrase is sent over plain
telnet. The passphrase path exists for convenience but is the least-recommended on an
unencrypted transport.

## 6. SSH public-key path

The cleanest credential model (encrypted + keypair = identity):

1. User registers an SSH public key on the website (`ssh_keys`).
2. User connects via SSH; the gate's SSH server authenticates by key, maps the fingerprint to
   an account — no password typed, whole session encrypted.
3. Unknown key -> fall back to interactive link-code entry, optionally offering to register the
   key to the just-authenticated account.

## 7. GMCP `Char.Login`

Rich clients can submit credentials structurally via GMCP `Char.Login` (code or passphrase)
instead of typing — same backends, nicer client UX (GMCP.md).

## 8. Transport encryption posture

The gate should listen on multiple transports (see §11 D1):

- **Plain telnet** (legacy clients) — works, but credentials/play are cleartext.
- **TLS telnet / TELNETS** (recommended default) — modern clients (Mudlet, etc.) support it;
  encrypts everything, makes the passphrase path safe.
- **SSH** — encrypted + key auth.

GMCP/MCCP negotiation is identical across transports; only the byte stream's encryption
differs.

## 9. Gate <-> world trust — signed session assertions

When the gate opens the `Play.Connect` stream ([PROTOCOL.md](PROTOCOL.md) §1), the world shard
needs to trust the asserted `account_id`/`character_id` **without** a synchronous call to
account on the hot path.

- On successful auth, account issues a **short-lived signed session assertion**
  `{account_id, character_id, session_id, exp}`, signed with account's private key.
- The gate carries it in the `Attach` frame; shards verify the signature against account's
  **published public key** (cached, rotated periodically) — fully offline, no per-connect RPC.
- This decouples the world from account at connect time and means a compromised gate can't
  forge identities it wasn't granted. Distinct from the intra-cluster handoff token.

## 10. Single-session lock

- On entering the world, acquire a **Redis lock per character** (one live session per
  character). The lock has a TTL and is heartbeated by the session, so a crashed connection's
  lock self-expires.
- **Conflict policy** (see §11 D3): a second login either **takes over** (the old connection
  is dropped — "you have been disconnected by a new login") or is **rejected**.
- This complements the `epoch` / `state_version` single-writer guard
  (PERSISTENCE.md §7): the session lock prevents two *connections*; epoch/version prevent two
  *shard owners*.

## 11. Schema additions

```sql
CREATE TABLE account_auth (
  account_id     UUID PRIMARY KEY REFERENCES accounts(id),
  passphrase_hash TEXT,                 -- Argon2id; null if passphrase auth not set
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ssh_keys (
  account_id  UUID NOT NULL REFERENCES accounts(id),
  fingerprint TEXT PRIMARY KEY,         -- SHA256 of the pubkey
  pubkey      TEXT NOT NULL,
  label       TEXT,
  added_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

(`accounts` and `account_identities` are defined in PERSISTENCE.md §2.)

## 12. Security checklist

- OAuth: PKCE, `state` CSRF guard, strict redirect-URI allowlist, store minimal provider data.
- No email-based account auto-merge.
- Link codes: high entropy, single-use, short TTL, bound to account.
- Passphrases: Argon2id, per-account + per-IP rate limits, lockout/backoff; discouraged over
  plain telnet.
- Session assertions: short-lived, signed, verified offline by shards.
- Single-session lock with TTL + heartbeat.
- Prefer encrypted transports; warn on cleartext credential entry.
