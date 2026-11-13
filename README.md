# aislop-server

The service behind `aislop.tikaydaes.dev`: a community database of AI-slop videos and channels on YouTube,
used by the [InvidioSlop](https://github.com/tikaydaes/invidioslop) browser extension.

One Go binary and a Postgres database.

## What it does

- **Anyone can pull.** `GET /v1/snapshot` is everything currently listed; `GET /v1/delta?since=N` is what
  changed since sequence number `N`. Nothing about what a user watches is ever sent.
- **Signed-in users judge videos.** One judgement per user per video: slop or not, which parts are AI
  (`voice`, `visual`, `script`, `music`) and a level from 1 to 3. Reporting, confirming and contesting are
  all the same record.
- **Consensus is weighted by reputation.** A video is listed when the weighted lead is at least 3, the
  leading side has at least twice the other's weight, and at least two different accounts are on it.
- **Channels follow their videos.** A channel is listed once 3 of its videos are confirmed, by at least 3
  different reporters, and at least 60% of its settled videos are confirmed.
- **Channel ID → @handle mappings** are contributed by clients and only served once independent sources
  agree (see below).

## Reputation

- Reputation runs from 0 to 100. Vote weight is `0.05 + 2.95 × √(rep/100)`, so a new account weighs 0.05
  and the ceiling is 3.
- When a video settles, people on the winning side gain up to 1 point, the first reporter 1.5×, and the
  losing side loses 1.5.
- **The gain comes from who you agreed with.** Each other winner contributes `their weight / 3`, divided by
  `1 + the number of times the two of you were already rewarded together`. Agreeing with throwaway accounts
  is worth almost nothing, and agreeing with the same partner pays less every time.
- If a video's outcome later flips, the earlier awards are reversed through a ledger (`rep_events`).
- New reports per day: `5 + 5 × reputation`, capped at 500.
- Reputation fades by about 1% a week.

The constants live at the top of `internal/server/videos.go`.

## Channel mappings

`POST /v1/channels/claims` records "channel `UC…` has handle `@x`" from a client that looked it up on
YouTube. `POST /v1/channels/lookup` returns confirmed mappings.

- A source is an account with reputation ≥ 5 (weight 1.5), or otherwise the caller's network: a salted
  hash of the /24 (IPv4) or /48 (IPv6). Many installs behind one network are one source.
- A mapping is served once one answer has weight ≥ 3 and at least twice the runner-up.
- It is withdrawn when dissenting sources reach weight 3 and outweigh its supporters, until one answer
  clearly wins again. Clients re-check a small random share of served mappings, which is how renames and
  bad entries are caught.
- Claims expire after 30 days. The salt is replaced every 30 days and old salts are deleted.

## Accounts

- Passkey only: no email, no password, no recovery. Up to 5 passkeys per account.
- Creating an account needs a small proof of work and is limited to 3 per network per day.
- The service is its own OAuth 2.1 server for one public client (`invidioslop`): authorization code with
  PKCE, 1-hour access tokens, single-use rotating refresh tokens. Replaying a refresh token ends that
  sign-in.

## Moderation

- `/admin` (administrators only): open contests, disputed videos, rulings on videos and channels, account
  reputation and barring.
- `/contest`: anyone can object to a listing without an account. It goes to the admin queue and changes
  nothing by itself.
- First administrator: set `ADMIN_TOKEN`, sign in, open `/admin/claim?token=…`, then remove the variable.

## Running

```
cp .env.example .env    # fill it in
docker compose up -d --build
```

| Variable | Meaning |
|---|---|
| `DATABASE_URL` | Postgres connection string (set by `compose.yaml`) |
| `PUBLIC_ORIGIN` | Public URL; also the passkey relying-party origin. Changing it invalidates every passkey. |
| `TRUSTED_PROXIES` | Proxies whose `X-Forwarded-For` is believed. Wrong values break per-network limits. |
| `OAUTH_REDIRECT_URIS` | Exact redirect URIs the extension may use |
| `ADMIN_TOKEN` | One-time secret for claiming the admin role |
| `POW_BITS` | Proof-of-work difficulty, default 18 |

The schema is applied at startup.

## Tests

The tests need an empty Postgres database that they are allowed to wipe:

```
TEST_DATABASE_URL=postgres://user@127.0.0.1:5432/aislop_test go test ./...
```

## License

AGPL-3.0. See [LICENSE](LICENSE).
