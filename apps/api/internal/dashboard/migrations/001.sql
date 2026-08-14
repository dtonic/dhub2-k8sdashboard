CREATE TABLE IF NOT EXISTS dashboard_schema_version (version integer PRIMARY KEY);
INSERT INTO dashboard_schema_version(version) VALUES (1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS dashboard_drafts (
  id uuid PRIMARY KEY, owner_sub text NOT NULL, revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
  state text NOT NULL CHECK (state IN ('draft','submitted','approved')),
  schema_version integer NOT NULL, definition jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS dashboard_drafts_owner_page ON dashboard_drafts(owner_sub, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS dashboard_drafts_review_page ON dashboard_drafts(state, created_at DESC, id DESC);
