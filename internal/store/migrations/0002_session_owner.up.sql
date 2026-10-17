-- Write protection. Until now anyone who knew a session ID could overwrite,
-- reactivate or end the session. The creator now receives a random owner
-- token; the server keeps only its SHA-256 here and every write must present
-- the token.
--
-- A session without a hash accepts no writes; the CLI starts a new session
-- (and link) when it meets one.
ALTER TABLE "sessions" ADD COLUMN IF NOT EXISTS "owner_token_hash" bytea;

-- Bytes of blob content stored for the session, after compression. Backs the
-- per-session storage quota without summing the blobs table on every write.
ALTER TABLE "sessions" ADD COLUMN IF NOT EXISTS "storage_bytes" bigint DEFAULT 0 NOT NULL;
