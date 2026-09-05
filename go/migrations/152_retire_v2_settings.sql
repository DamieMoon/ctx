-- =============================================================================
-- 152_retire_v2_settings.sql — die Bestands-Rows der zweiten Retirement-Vintage
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Tree-Shaking-Run 2026-09, Welle T05-10 (design/05 §3, DECISIONS.md K32, K1).
-- Bauform und ausführliche Begründung: 133_retire_backend_tuple_rows.sql — was
-- dort für die 29 Backend-Tupel steht, gilt hier mechanisch gleich; wiederholt
-- wird nur, was für DIESE Vintage anders ist.
--
-- WAS: die context_settings-Zeilen der zweiten Retirement-Vintage, in ALLEN
-- Scopes — distill.local_only (Welle T05-8a) und root_map.label_budget
-- (Welle T05-8c). Vorbedingung erfüllt: beide Namen sind aus der Registry
-- geschnitten (Pin: internal/config/retired_test.go,
-- TestRetiredV2KeysLeftTheRegistry).
--
-- WARUM DIESE VINTAGE EIGEN IST: die 29 Keys von 133 hatten ein Ziel — ihr
-- Wert zog in den Backend-Pool um. Diese beiden haben KEINEN Nachfolger: der
-- Wert zieht nicht um, er hört auf zu existieren (die Ist-Sätze je Key stehen
-- in config.retiredKeysWithoutSuccessor, internal/config/retired.go, und
-- werden hier nicht abgeschrieben). Daraus folgt, was unten fehlt: kein
-- Upgrade-Hop-Hinweis wie in 133, kein Verweis auf v5.0.0 und keiner auf einen
-- Pool. Die Keys verschwinden in v5.16.0.
--
-- WARUM EIN DELETE UND KEIN LIEGENLASSEN: eine Row auf einem unregistrierten
-- Key ist nicht dauerhaft harmlos, sondern nur so lange, wie niemand den Namen
-- re-registriert — admitOverride admittet jeden registrierten Key
-- (internal/settings/build.go). Bis dahin ist sie unsichtbar: HandleList
-- rendert rein registry-getrieben, die Secrets-API filtert über config.Keys(),
-- und eine tenant-skopierte Zeile erreicht nicht einmal den globalen Reload.
-- Der Boot-Row-Sweep (cmd/ctxd/main.go, warnRetiredV2SettingRowsBoot) macht
-- sie sichtbar, er räumt sie nicht weg — das tut diese Datei, vor dem ersten
-- Serve jedes künftigen Binaries.
--
-- KEIN SCOPE-FILTER: beide Keys waren mut:"hot" (internal/config/config.go)
-- und damit auf jeder Installation per PUT /api/settings in jedem Scope
-- schreibbar. Ein WHERE scope = '_global' ließe genau die Klasse zurück, die
-- der Boot-Sweep am schlechtesten erreicht — der volle settings.Reload lädt
-- ausschließlich '_global'. Das Integrations-Gate probt den Tenant-Scope
-- explizit (cmd/ctxd/settings_retire_v2_integration_test.go).
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
--      WHERE metadata->>'request_id' = 'migration-152-retire-v2-settings';
--
-- DIE MELDUNG feuert NUR bei > 0 gelöschten Rows: ein Fresh-Install hat nichts
-- zu räumen und bekommt keine Meldung. Englisch wie die Meldungen aus 133 und
-- wie docs/. Transport: der OnNotice-Handler in internal/store.NewPool — ohne
-- ihn verwirft pgx jede NOTICE stumm.
--
-- IDEMPOTENT UND TRANSAKTIONAL: der Runner fährt jede Datei in einer eigenen
-- Transaktion; ein DELETE auf leerer Treffermenge ist ein no-op ohne Meldung.
-- Der Runner verifiziert Checksummen bereits applieder Versionen NICHT (nur
-- EXISTS-Skip) — daraus die Bau-Regel: diese Datei wird nach dem Release nie
-- editiert. Ein nachträglich retirierter Key ist eine NEUE Migration (K1;
-- K32: die drei hermes-Keys bekommen 153+, falls T01-8 je läuft).
--
-- lock_timeout (Muster 119/129/133): ohne ihn wartet die Migration hinter
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
--   config.retiredKeysWithoutSuccessor die zweite Transkription derselben
--   Namen. Beide Richtungen sind gepinnt
--   (internal/config/retiredv2migration_test.go, Set-Gleichheit gegen
--   config.RetiredV2KeyNames()): ein verlorener Eintrag ließe eine Row-Klasse
--   still liegen, ein überschüssiger löschte Rows eines LEBENDEN Keys.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Attribuiert den Bulk-unset in context_settings_audit.metadata.request_id.
-- Muss vor dem DELETE stehen und in derselben Transaktion liegen — der
-- Audit-Trigger liest current_setting('ctx.request_id', true) je Row.
SET LOCAL ctx.request_id = 'migration-152-retire-v2-settings';

DO $$
DECLARE
    v_deleted INT;
BEGIN
    -- Alle Scopes. Sortiert wie config.RetiredV2KeyNames(), damit der
    -- Drift-Pin und ein git-diff dieselbe Reihenfolge sehen.
    DELETE FROM context_settings
     WHERE key = ANY (ARRAY[
        'distill.local_only',
        'root_map.label_budget'
     ]);

    GET DIAGNOSTICS v_deleted = ROW_COUNT;

    IF v_deleted > 0 THEN
        RAISE NOTICE 'migration 152: deleted % context_settings row(s) on the retired keys distill.local_only and root_map.label_budget, across all scopes. Both were retired in v5.16.0 WITHOUT a successor — no pool and no other key owns those values now, and nothing reads them.', v_deleted;
        RAISE NOTICE 'migration 152: the deleted values survive as audit history — SELECT entity_key, scope, old_value FROM context_settings_audit WHERE metadata->>''request_id'' = ''migration-152-retire-v2-settings'';';
    END IF;
END $$;
