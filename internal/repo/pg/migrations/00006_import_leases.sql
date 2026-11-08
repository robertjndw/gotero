-- +goose Up
-- A worker owns a running import only while its lease lasts. The token in
-- claimed_by guards every write the worker makes, so a process that stalled
-- past its lease cannot overwrite the job once another one took it over.
-- Running jobs from before this migration have no lease and count as expired.
ALTER TABLE import_jobs
    ADD COLUMN claimed_by       text,
    ADD COLUMN lease_expires_at timestamptz;

-- Earlier versions let one account queue several imports, which then wrote
-- into the same library at once. Keep the oldest active job per account so
-- the index below can be built.
UPDATE import_jobs j
SET status = 'cancelled', finished_at = now(), source_api_key = NULL,
    error = 'Cancelled because another import for this account was already queued.'
WHERE j.status IN ('queued', 'running')
  AND EXISTS (
    SELECT 1 FROM import_jobs o
    WHERE o.user_id = j.user_id AND o.status IN ('queued', 'running') AND o.id < j.id
  );

-- Every job imports into the account's personal library, so one active job
-- per user is one active job per target library.
CREATE UNIQUE INDEX import_jobs_one_active_idx ON import_jobs (user_id)
    WHERE status IN ('queued', 'running');

-- +goose Down
DROP INDEX import_jobs_one_active_idx;
ALTER TABLE import_jobs DROP COLUMN lease_expires_at, DROP COLUMN claimed_by;
