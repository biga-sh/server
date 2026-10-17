-- The session store: sessions, their current tree, content-addressed blobs,
-- append-only checkpoints and the head version of every file. Statements are
-- IF NOT EXISTS so an existing biga database is adopted as is.

CREATE TABLE IF NOT EXISTS "sessions" (
	"id" text PRIMARY KEY NOT NULL,
	"title" text NOT NULL,
	"description" text,
	"status" text DEFAULT 'active' NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	"updated_at" timestamp with time zone DEFAULT now() NOT NULL,
	"ended_at" timestamp with time zone,
	"group_id" text,
	CONSTRAINT "sessions_status_chk" CHECK ("status" in ('active', 'ended'))
);
CREATE INDEX IF NOT EXISTS "sessions_group_id" ON "sessions" USING btree ("group_id");
CREATE INDEX IF NOT EXISTS "sessions_status_updated" ON "sessions" USING btree ("status", "updated_at");
CREATE INDEX IF NOT EXISTS "sessions_status_ended" ON "sessions" USING btree ("status", "ended_at");

CREATE TABLE IF NOT EXISTS "snapshots" (
	"session_id" text PRIMARY KEY NOT NULL,
	"tree" jsonb NOT NULL,
	"sequence_num" integer DEFAULT 0 NOT NULL,
	"timestamp" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "snapshots_sequence_num_chk" CHECK ("sequence_num" >= 0),
	CONSTRAINT "snapshots_session_id_sessions_id_fk" FOREIGN KEY ("session_id")
		REFERENCES "sessions"("id") ON DELETE cascade
);

CREATE TABLE IF NOT EXISTS "blobs" (
	"session_id" text NOT NULL,
	"hash" text NOT NULL,
	"content" bytea NOT NULL,
	"compression" text DEFAULT 'none' NOT NULL,
	"size" integer NOT NULL,
	"binary" boolean DEFAULT false NOT NULL,
	CONSTRAINT "blobs_session_id_hash_pk" PRIMARY KEY ("session_id", "hash"),
	CONSTRAINT "blobs_size_chk" CHECK ("size" >= 0),
	CONSTRAINT "blobs_session_id_sessions_id_fk" FOREIGN KEY ("session_id")
		REFERENCES "sessions"("id") ON DELETE cascade
);
-- Most rows are already brotli-compressed; skip Postgres' own pglz pass.
ALTER TABLE "blobs" ALTER COLUMN "content" SET STORAGE EXTERNAL;

CREATE TABLE IF NOT EXISTS "checkpoints" (
	"id" serial PRIMARY KEY NOT NULL,
	"session_id" text NOT NULL,
	"seq" integer NOT NULL,
	"label" text NOT NULL,
	"additions" integer DEFAULT 0 NOT NULL,
	"deletions" integer DEFAULT 0 NOT NULL,
	"files_changed" integer DEFAULT 0 NOT NULL,
	"created_at" timestamp with time zone DEFAULT now() NOT NULL,
	CONSTRAINT "checkpoints_session_seq" UNIQUE ("session_id", "seq"),
	CONSTRAINT "checkpoints_counts_chk" CHECK ("seq" >= 0 and "additions" >= 0 and "deletions" >= 0 and "files_changed" >= 0),
	CONSTRAINT "checkpoints_session_id_sessions_id_fk" FOREIGN KEY ("session_id")
		REFERENCES "sessions"("id") ON DELETE cascade
);

CREATE TABLE IF NOT EXISTS "checkpoint_changes" (
	"id" serial PRIMARY KEY NOT NULL,
	"session_id" text NOT NULL,
	"checkpoint_seq" integer NOT NULL,
	"path" text NOT NULL,
	"change_type" text NOT NULL,
	"old_path" text,
	"before_hash" text,
	"after_hash" text,
	"additions" integer DEFAULT 0 NOT NULL,
	"deletions" integer DEFAULT 0 NOT NULL,
	"binary" boolean DEFAULT false NOT NULL,
	CONSTRAINT "checkpoint_changes_type_chk" CHECK ("change_type" in ('added', 'modified', 'deleted', 'renamed')),
	CONSTRAINT "checkpoint_changes_after_hash_chk" CHECK (("change_type" = 'deleted') = ("after_hash" is null)),
	CONSTRAINT "checkpoint_changes_counts_chk" CHECK ("additions" >= 0 and "deletions" >= 0),
	CONSTRAINT "checkpoint_changes_session_id_sessions_id_fk" FOREIGN KEY ("session_id")
		REFERENCES "sessions"("id") ON DELETE cascade,
	CONSTRAINT "checkpoint_changes_ckpt_fk" FOREIGN KEY ("session_id", "checkpoint_seq")
		REFERENCES "checkpoints"("session_id", "seq") ON DELETE cascade
);
CREATE INDEX IF NOT EXISTS "checkpoint_changes_session_seq" ON "checkpoint_changes" USING btree ("session_id", "checkpoint_seq");
CREATE INDEX IF NOT EXISTS "checkpoint_changes_session_path_seq" ON "checkpoint_changes" USING btree ("session_id", "path", "checkpoint_seq");

CREATE TABLE IF NOT EXISTS "files" (
	"session_id" text NOT NULL,
	"path" text NOT NULL,
	"hash" text NOT NULL,
	"size" integer NOT NULL,
	"binary" boolean DEFAULT false NOT NULL,
	CONSTRAINT "files_session_id_path_pk" PRIMARY KEY ("session_id", "path"),
	CONSTRAINT "files_size_chk" CHECK ("size" >= 0),
	CONSTRAINT "files_session_id_sessions_id_fk" FOREIGN KEY ("session_id")
		REFERENCES "sessions"("id") ON DELETE cascade,
	CONSTRAINT "files_blob_fk" FOREIGN KEY ("session_id", "hash")
		REFERENCES "blobs"("session_id", "hash") ON DELETE cascade
);
