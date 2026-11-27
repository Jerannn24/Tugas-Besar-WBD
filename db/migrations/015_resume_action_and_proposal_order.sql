
CREATE INDEX IF NOT EXISTS migration_proposals_created_cursor_idx
    ON app.migration_proposals (database_id, created_at DESC, id DESC);

CREATE OR REPLACE FUNCTION app.workflow_source_valid(db uuid, target uuid, migration uuid, rev integer, action_name text, source uuid, execution_mode text)
RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT CASE
      WHEN action_name = 'apply' THEN source IS NULL
      WHEN source IS NULL THEN false
      WHEN execution_mode = 'batch' AND action_name IN ('resume', 'compensate') THEN EXISTS (
        SELECT FROM app.jobs j WHERE j.execution_id = source AND j.database_id = db AND j.target_id = target
          AND j.migration_id = migration AND j.revision = rev AND j.mode = 'batch'
          AND CASE action_name
            WHEN 'resume' THEN (j.direction = 'up' AND j.status IN ('failed', 'partial', 'unknown'))
                OR (j.action = 'compensate' AND j.direction = 'down' AND j.status = 'partially_reverted')
            ELSE j.direction = 'up' AND (j.status IN ('partial', 'partially_reverted') OR (
                j.status = 'succeeded' AND EXISTS (SELECT FROM app.migration_active_heads h
                    WHERE h.database_id = db AND h.active_head_execution_id = source))) END
      ) AND NOT EXISTS (
        SELECT FROM app.jobs resumed
        WHERE resumed.database_id = db
          AND resumed.target_id = target
          AND resumed.migration_id = migration
          AND resumed.revision = rev
          AND resumed.mode = 'batch'
          AND resumed.action = 'resume'
          AND resumed.source_execution_id = source
          AND resumed.status = 'succeeded'
      ) AND NOT EXISTS (
        SELECT FROM app.migration_history_entries h
        WHERE h.database_id = db AND h.is_complete AND h.reverts_execution_id = source
      )
      WHEN execution_mode = 'standard' AND action_name = 'rollback' THEN EXISTS (
        SELECT FROM app.migration_active_heads head
        JOIN app.migration_history_entries h ON h.database_id = head.database_id AND h.execution_id = head.active_head_execution_id
        JOIN app.jobs j ON j.id = h.job_id
        WHERE head.database_id = db AND h.execution_id = source AND h.migration_id = migration AND h.revision = rev
          AND h.is_complete AND h.direction = 'up' AND j.target_id = target
          AND length(trim(COALESCE(j.snapshot->>'sql_down', ''))) > 0
      )
      WHEN action_name = 'forward' THEN source = (
        SELECT h.reverts_execution_id FROM app.migration_history_entries h
        JOIN app.jobs j ON j.id = h.job_id AND j.target_id = target
        WHERE h.database_id = db AND h.is_complete AND h.reverts_execution_id IS NOT NULL
          AND h.version > COALESCE((SELECT max(a.version) FROM app.migration_history_entries a
            WHERE a.database_id = db AND a.is_complete AND a.action = 'apply' AND a.direction = 'up'), 0)
          AND NOT EXISTS (SELECT FROM app.migration_history_entries f WHERE f.database_id = db AND f.is_complete
            AND f.action = 'forward' AND f.source_execution_id = h.reverts_execution_id AND f.version > h.version)
        ORDER BY h.version DESC LIMIT 1
      ) AND EXISTS (SELECT FROM app.jobs j WHERE j.execution_id = source AND j.database_id = db
          AND j.target_id = target AND j.migration_id = migration AND j.revision = rev
          AND j.mode = execution_mode AND j.direction = 'up')
      ELSE false END;
$$;
GRANT EXECUTE ON FUNCTION app.workflow_source_valid(uuid, uuid, uuid, integer, text, uuid, text) TO wbd_platform_runtime;
