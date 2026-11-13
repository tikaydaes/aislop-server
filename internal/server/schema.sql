-- Applied at startup; every statement is idempotent.

CREATE TABLE IF NOT EXISTS meta (
  k text PRIMARY KEY,
  v text NOT NULL
);

CREATE SEQUENCE IF NOT EXISTS change_seq;

-- Accounts are pseudonymous: a random id, a display name and passkeys.
CREATE TABLE IF NOT EXISTS users (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  wa_handle   bytea NOT NULL UNIQUE,              -- WebAuthn user handle
  name        text NOT NULL,
  reputation  double precision NOT NULL DEFAULT 0, -- 0..100
  is_admin    boolean NOT NULL DEFAULT false,
  banned      boolean NOT NULL DEFAULT false,
  net_key     text NOT NULL DEFAULT '',            -- hashed network at signup (throttling only)
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS credentials (
  id          bytea PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  data        jsonb NOT NULL,                      -- webauthn.Credential
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS credentials_user ON credentials (user_id);

-- Short-lived server-side state: web sessions, WebAuthn ceremonies,
-- OAuth authorization codes, spent proof-of-work challenges.
CREATE TABLE IF NOT EXISTS ephemeral (
  id          text PRIMARY KEY,                    -- sha256 of the secret
  kind        text NOT NULL,
  user_id     uuid REFERENCES users ON DELETE CASCADE,
  data        jsonb NOT NULL DEFAULT '{}',
  expires_at  timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS ephemeral_expiry ON ephemeral (expires_at);

CREATE TABLE IF NOT EXISTS tokens (
  hash        text PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  kind        text NOT NULL,                       -- access | refresh
  family      uuid NOT NULL,                       -- one per sign-in; revoked together
  used        boolean NOT NULL DEFAULT false,      -- refresh tokens are single-use
  expires_at  timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS tokens_family ON tokens (family);
CREATE INDEX IF NOT EXISTS tokens_expiry ON tokens (expires_at);

-- handle: NULL = not confirmed yet (not served), '' = confirmed to have none.
CREATE TABLE IF NOT EXISTS channels (
  uc          text PRIMARY KEY,
  handle      text,
  name        text NOT NULL DEFAULT '',
  flagged     boolean NOT NULL DEFAULT false,
  override    smallint NOT NULL DEFAULT 0,          -- admin: 1 force flag, -1 force clear
  n_confirmed integer NOT NULL DEFAULT 0,
  n_rejected  integer NOT NULL DEFAULT 0,
  seq         bigint NOT NULL DEFAULT 0,            -- >0 once it has ever been published
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS channels_seq ON channels (seq) WHERE seq > 0;

-- One live claim per source per channel. A source is a reputable account
-- or, for everyone else, a hashed network prefix.
CREATE TABLE IF NOT EXISTS channel_claims (
  uc          text NOT NULL,
  source      text NOT NULL,
  handle      text NOT NULL,
  name        text NOT NULL DEFAULT '',
  weight      real NOT NULL,
  seen_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (uc, source)
);
CREATE INDEX IF NOT EXISTS channel_claims_seen ON channel_claims (seen_at);

-- status: 0 pending, 1 confirmed slop, 2 rejected.
CREATE TABLE IF NOT EXISTS videos (
  id          text PRIMARY KEY,
  uc          text NOT NULL DEFAULT '',
  status      smallint NOT NULL DEFAULT 0,
  types       smallint NOT NULL DEFAULT 0,          -- bit 1 voice, 2 visual, 4 script, 8 music
  level       smallint NOT NULL DEFAULT 0,          -- 1..3
  yes         real NOT NULL DEFAULT 0,
  no          real NOT NULL DEFAULT 0,
  n_yes       integer NOT NULL DEFAULT 0,
  n_no        integer NOT NULL DEFAULT 0,
  override    smallint NOT NULL DEFAULT 0,          -- admin: 1 confirm, -1 reject
  first_user  uuid,
  seq         bigint NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS videos_seq ON videos (seq) WHERE seq > 0;
CREATE INDEX IF NOT EXISTS videos_uc ON videos (uc);
CREATE INDEX IF NOT EXISTS videos_pending ON videos (updated_at) WHERE status = 0;

CREATE TABLE IF NOT EXISTS assessments (
  user_id     uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  video_id    text NOT NULL REFERENCES videos ON DELETE CASCADE,
  slop        boolean NOT NULL,
  types       smallint NOT NULL DEFAULT 0,
  level       smallint NOT NULL DEFAULT 0,
  uc          text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, video_id)
);
CREATE INDEX IF NOT EXISTS assessments_video ON assessments (video_id);
CREATE INDEX IF NOT EXISTS assessments_user_time ON assessments (user_id, created_at);

-- Ledger of every reputation change, so outcomes can be reversed if a
-- video's status later flips and so history is auditable.
CREATE TABLE IF NOT EXISTS rep_events (
  id          bigserial PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  video_id    text,
  delta       double precision NOT NULL,
  reason      text NOT NULL,
  reversed    boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS rep_events_video ON rep_events (video_id) WHERE NOT reversed;
CREATE INDEX IF NOT EXISTS rep_events_user ON rep_events (user_id, id);

-- How often two users have been rewarded for agreeing; a < b.
CREATE TABLE IF NOT EXISTS pair_counts (
  a uuid NOT NULL,
  b uuid NOT NULL,
  n integer NOT NULL DEFAULT 0,
  PRIMARY KEY (a, b)
);

CREATE TABLE IF NOT EXISTS contests (
  id          bigserial PRIMARY KEY,
  target      text NOT NULL,
  reason      text NOT NULL,
  contact     text NOT NULL DEFAULT '',
  net_key     text NOT NULL DEFAULT '',
  status      text NOT NULL DEFAULT 'open',
  created_at  timestamptz NOT NULL DEFAULT now()
);
