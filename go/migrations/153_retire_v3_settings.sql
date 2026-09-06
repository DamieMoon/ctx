-- =============================================================================
-- 153_retire_v3_settings.sql — die Bestands-Rows der dritten Retirement-Vintage
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Tree-Shaking-Nachzug 2026-09, Welle NZ-5 (DECISIONS.md Teil 2 N1, Teil 1 K32,
-- K1). Bauform und ausführliche Begründung: 152_retire_v2_settings.sql — was
-- dort für die zwei Keys ohne Nachfolger steht, gilt hier mechanisch gleich;
-- wiederholt wird nur, was für DIESE Vintage anders ist.
--
-- WAS: die context_settings-Zeilen der dritten Retirement-Vintage, in ALLEN
-- Scopes — distill.session_quiet_for, distill.source_label und
-- distill.source_path (Welle NZ-5). Vorbedingung erfüllt: alle drei Namen sind
-- aus der Registry geschnitten (Pin: internal/config/retired_test.go,
-- TestRetiredV3KeysLeftTheRegistry).
--
-- WARUM DIESE VINTAGE EIGEN IST — ZWEI GRÜNDE, die zusammenfallen:
--
--   1. DER PIN. Die vorige Vintage ist per Mengengleichheit an ihre eigene
--      Migration gebunden (internal/config/retiredv2migration_test.go). Diese
--      Datei ist gelandet, appliziert und eingefroren; drei weitere Namen in
--      ihrer Liste hätten den Pin rot gemacht, ohne Reparaturweg. Vintage =
--      Release-Kohorte + Migrations-Pin (K19), und die Kohorte ist hier
--      v5.17.0, nicht die davor.
--
--   2. DER GRUND DES RUHESTANDS IST EIN ANDERER. Die Keys der vorigen Vintage
--      hatten keinen Nachfolger, weil ihr WERT nirgends hinzog. Diese drei
--      haben keinen Nachfolger, weil ihr SUBJEKT weg ist: sie konfigurierten
--      den Leser über die state.db eines fremden Agent-Runtimes
--      (internal/hermesstate samt Adapter), und dieser Zweig ist in v5.17.0
--      als Ganzes gefallen. Der Destillier-Arm lebt weiter, mit der Quelle
--      über den eigenen Korpus (internal/distillsource/ctxcheckpoint).
--
-- KEINE VERWECHSLUNG MIT distill.ctx_*: distill.ctx_source_label und
-- distill.ctx_quiet_for sind KEINE Umbenennungen von distill.source_label und
-- distill.session_quiet_for. Sie sind die immer schon eigenen Keys der zweiten
-- Quelle (seit A02-4) und stehen unverändert in der Registry. Ein Operator,
-- der hier einen Rename vermutet, würde einen laufenden Wasserzeichen-Strang
-- umbenennen — deshalb steht der Satz auch in der Meldung unten und in
-- docs/operations.md.
--
-- KEIN SCOPE-FILTER: alle drei waren mut:"hot" (internal/config/config.go) und
-- damit auf jeder Installation per PUT /api/settings in jedem Scope schreibbar.
-- Ein WHERE scope = '_global' ließe genau die Klasse zurück, die der
-- Boot-Sweep am schlechtesten erreicht — der volle settings.Reload lädt
-- ausschließlich '_global'. Das Integrations-Gate probt den Tenant-Scope
-- explizit (cmd/ctxd/settings_retire_v3_integration_test.go).
--
-- KEIN SCHEMA-CHANGE: context_settings, context_settings_audit und beide
-- Trigger bleiben byte-identisch. T07 (test.sh) zählt Tabellen/Spalten und
-- bleibt unverändert.
--
-- DIE AUDIT-ROWS SCHREIBT DER TRIGGER, NICHT DIESE DATEI: trg_settings_audit
-- legt pro gelöschter Row eine Zeile action='unset' an, old_value = der
-- gelöschte Wert, via='sql' (kein ctx.api_key_id gesetzt), metadata.request_id
-- aus dem SET LOCAL unten. Das ist der EINZIGE maschinenlesbare Rückweg nach
-- einem Binary-Rollback:
--
--     SELECT entity_key, scope, old_value
--       FROM context_settings_audit
--      WHERE metadata->>'request_id' = 'migration-153-retire-v3-settings';
--
-- DIE MELDUNG feuert NUR bei > 0 gelöschten Rows: ein Fresh-Install hat nichts
-- zu räumen und bekommt keine Meldung. Englisch wie die Meldungen der Vintages
-- davor und wie docs/. Transport: der OnNotice-Handler in internal/store.NewPool
-- — ohne ihn verwirft pgx jede NOTICE stumm.
--
-- IDEMPOTENT UND TRANSAKTIONAL: der Runner fährt jede Datei in einer eigenen
-- Transaktion; ein DELETE auf leerer Treffermenge ist ein no-op ohne Meldung.
-- Der Runner verifiziert Checksummen bereits applieder Versionen NICHT (nur
-- EXISTS-Skip) — daraus die Bau-Regel: diese Datei wird nach dem Release nie
-- editiert. Ein nachträglich retirierter Key ist eine NEUE Migration (K1).
--
-- lock_timeout (Muster 119/129/133/152): ohne ihn wartet die Migration hinter
-- einem fremden Lock auf context_settings unbegrenzt und hängt den Boot; mit
-- ihm scheitert sie laut (55P03), die Transaktion rollt zurück, die Version
-- bleibt unverzeichnet und der nächste Boot fährt sie erneut.
--
-- ⚠ KEINE T07-NACHZIEHUNG (die Datei ändert weder Tabelle noch Spalte), ABER
--   das Schema-Contract-Manifest zählt Migrationen mit und muss regeneriert
--   werden:
--   go test -tags=genmanifest ./internal/schemacontract -run TestGenerateManifest
--
-- ⚠ ZWEITE AUFZÄHLUNG: die Liste unten ist neben
--   config.retiredKeysWithoutSuccessorV3 die zweite Transkription derselben
--   Namen. Beide Richtungen sind gepinnt
--   (internal/config/retiredv3migration_test.go, Set-Gleichheit gegen
--   config.RetiredV3KeyNames()): ein verlorener Eintrag ließe eine Row-Klasse
--   still liegen, ein überschüssiger löschte Rows eines LEBENDEN Keys.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Attribuiert den Bulk-unset in context_settings_audit.metadata.request_id.
-- Muss vor dem DELETE stehen und in derselben Transaktion liegen — der
-- Audit-Trigger liest current_setting('ctx.request_id', true) je Row.
SET LOCAL ctx.request_id = 'migration-153-retire-v3-settings';

DO $$
DECLARE
    v_deleted INT;
BEGIN
    -- Alle Scopes. Sortiert wie config.RetiredV3KeyNames(), damit der
    -- Drift-Pin und ein git-diff dieselbe Reihenfolge sehen.
    DELETE FROM context_settings
     WHERE key = ANY (ARRAY[
        'distill.session_quiet_for',
        'distill.source_label',
        'distill.source_path'
     ]);

    GET DIAGNOSTICS v_deleted = ROW_COUNT;

    IF v_deleted > 0 THEN
        RAISE NOTICE 'migration 153: deleted % context_settings row(s) on the retired keys distill.session_quiet_for, distill.source_label and distill.source_path, across all scopes. All three configured the reader over a foreign agent runtime state database, which was removed whole in v5.17.0 — the distiller arm keeps running on its source over this store''s own compaction checkpoints.', v_deleted;
        RAISE NOTICE 'migration 153: distill.ctx_source_label and distill.ctx_quiet_for are NOT renames of the deleted keys — they are the second source''s own keys and are untouched. Do not copy a deleted value into them: one label is one watermark series.';
        RAISE NOTICE 'migration 153: the deleted values survive as audit history — SELECT entity_key, scope, old_value FROM context_settings_audit WHERE metadata->>''request_id'' = ''migration-153-retire-v3-settings'';';
    END IF;
END $$;
