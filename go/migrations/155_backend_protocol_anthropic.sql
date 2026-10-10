-- =============================================================================
-- 155_backend_protocol_anthropic.sql — 'anthropic' joins the backend wire
-- protocols
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- The baseline (113, context_backends) pins the protocol column to the three
-- wires the daemon spoke when the pool was designed: openai, ollama, rerank.
-- The native Anthropic Messages API wire (go/internal/llm/anthropic.go) is a
-- fourth; without this migration the CHECK rejects the row the handler's
-- validation already accepted, and the create fails one layer too late with
-- a bare constraint error.
--
-- The constraint was declared inline and unnamed in the baseline, so its name
-- is whatever the server assigned (context_backends_protocol_check on every
-- PostgreSQL this project supports). The DO block resolves it from the catalog
-- by table + definition instead of trusting the derived name, so a database
-- whose constraint was ever renamed by hand migrates the same way.
--
-- Nothing else changes: the column keeps its DEFAULT 'openai', the schema
-- manifest records column types only (no CHECK clauses), and existing rows are
-- untouched. Rolling back the daemon to a build without the anthropic wire
-- leaves such a row in the table; it would then dispatch onto the Ollama path
-- (pre-155 fallthrough) — delete or disable anthropic rows before downgrading.

DO $$
DECLARE
    con_name text;
BEGIN
    SELECT conname INTO con_name
      FROM pg_constraint
     WHERE conrelid = 'context_backends'::regclass
       AND contype = 'c'
       AND pg_get_constraintdef(oid) LIKE '%protocol%';
    IF con_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE context_backends DROP CONSTRAINT %I', con_name);
    END IF;
    ALTER TABLE context_backends
        ADD CONSTRAINT context_backends_protocol_check
        CHECK (protocol IN ('openai','ollama','rerank','anthropic'));
END
$$;
