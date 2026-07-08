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

## 8. Builder trust tiers & bootstrap admin

Accounts carry a **tier** — the account-level role that gates builder/admin powers (`accounts.tier`, #27).
The default ladder is `player < builder < admin`, but the ladder is **content-defined** (#29): a pack may
declare its own tiers (`trust_tiers`), each with an ordinal **rank** and a set of granted **capability
flags** (`holylight`, `builder`, `admin`). `telos-account` is the authority and signs the tier into the
session assertion so the world trusts it offline. A promote/demote flow (admin-gated) changes a tier; the
change takes effect on the target's next login (the assertion re-signs the tier) and survives a cross-shard
walk (the tier rides the *signed* handoff snapshot, #106).

**Promote / demote.** An admin changes another account's tier with the edge verbs `promote <character>
<tier>` and `demote <character>` (→ the ladder's **baseline**, i.e. its lowest-rank tier — *not* a hardcoded
`player`, since a pack may rename it, #112). Authorization is enforced at the account service (the actor's
tier is read from the store; a non-admin is refused), never at the edge. Every change is audited.

**Authorization ceilings (#165).** An actor may act only within its own standing, on **two independent
axes**:

- **Rank** — may not grant a tier ranked above its own, nor change an account ranked above it.
- **Capability** — may not **grant** a tier holding a capability its own tier lacks (unconditional — writing
  a tier mints its capabilities), nor **change a same-rank** account holding one. Rank and capability are
  independent, so the rank ceiling alone is insufficient: a same-rank tier with a richer flag set, or a
  lower-rank tier with orthogonal flags, would otherwise let an actor mint a capability it never held.

The capability ceiling is asymmetric by design: the **grant** side is universal, but the **target** side
fires only at equal rank. A strictly-higher actor may always manage a lower one — otherwise a ladder whose
top tier is not a capability superset of everything beneath it would strand accounts no one could demote.

> **Authoring constraint (a `LintTrustLadder` guard, #111).** For a ladder to be fully manageable, each
> admin-capable tier should be a **capability superset** of every tier its rank can reach — else it can
> *demote* a richer lower tier but never *create* one (the lint warns). A **baseline** tier granting any
> capability elevates the entire playerbase and is **rejected** (logged at boot, refused at a fleet reload),
> as are duplicate ranks and nameless rungs.

> **Ladder consistency.** `telos-account` and the world resolve their pack set through the **same** resolver
> (`content.ResolveEnabledPacks` over the `content_pack_registry`), so for a given DB state they load an
> identical ladder — closing the divergence that let a pack-authored tier escalate builder→admin (#246). On a
> real content/registry read error `telos-account` **fails closed**: `SetAccountTier` returns `Unavailable` and
> refuses every tier change (the break-glass CLI still works — it bypasses the service). Two operator caveats:
> - **`TELOS_CONTENT_PACKS` must be identical on the world and telos-account** (or set on neither). The shared
>   resolver makes the *logic* identical, but this explicit override is read from each process's own env — a
>   mismatch re-opens the divergence. Prefer leaving it unset so both follow the registry.
> - A content **reload** still needs a telos-account restart to take effect (the ladder is boot-pinned, #248).
>
> The account gRPC listener requires a shared **caller token** (`TELOS_ACCOUNT_CALLER_TOKEN`), so only the gate
> can reach the privileged RPCs; without it the listener would accept an unauthenticated `actor_account_id` (or
> mint an assertion for any account) from anyone who can dial it (#247).

> **Last-admin lockout recovery (#108).** There is no self/last-admin demote guard, so an admin *can* demote
> the last admin. Config-pin (`TELOS_BOOTSTRAP_ADMIN`) does **not** recover this — it applies only at account
> *creation*, so re-setting it is a no-op for an existing account. The sanctioned recovery is the break-glass
> CLI, run by whoever has DB/host access (which **is** the authorization):
>
> ```
> telos-account set-tier --character <name> --tier admin
> ```
>
> It forces the tier directly, bypassing the in-game admin check and the promote ceilings, and audits the
> change with a system (NULL) actor. It validates the tier against the loaded ladder unless `--force yes`.
> Treat the pinned owner's account with care.

**Bootstrap admin (config-pin).** Set `TELOS_BOOTSTRAP_ADMIN` to your OAuth **login**; the FIRST account
created for a matching identity is granted `admin` (audited with a NULL actor). The match is **login-only,
by design** — a login is unique and provider-verified, whereas the OAuth email is a public, user-settable,
unverified provider field, so matching on email would let anyone who set that email to the pinned value
claim admin. Because config-pin names a single verified login, there is no connect-race. Empty/unset pin =
no account is ever auto-admin'd. A dev-autoauth (`TELOS_DEV_AUTOAUTH`) bare-name login has no OAuth
identity, so it never hits this path.

## 9. Security checklist

- OAuth: PKCE, `state` CSRF guard, strict redirect-URI allowlist, store minimal provider data.
- No email-based account auto-merge — identity is `(provider, provider_uid)` only.
- Bootstrap admin: pin matches the OAuth login only (never the unverified provider email).
- Device codes: high entropy (256-bit), single-use, short TTL (~10 min).
- Session assertions: short-lived, signed, verified offline by shards.
- Single-session lock with TTL + heartbeat; takeover drops the displaced connection.
- Prefer the encrypted (TLS) transport.
