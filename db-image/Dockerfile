# pgvector + TimescaleDB on PostgreSQL 18 (Debian trixie)
# Base: pgvector/pgvector:0.8.7-pg18-trixie (includes pgvector extension)
# Added: TimescaleDB from packagecloud timescale/timescaledb repo
# pgvector fest gepinnt (E-01-4): 0.8.3+ fixt die PG18-Distanz-Regression und die
# HNSW-Vacuum-Korruption. 0.8.6 (Sweep 2026-09-19) bringt nur IVFFlat-/sparsevec-
# Fixes, die wir nicht nutzen — der Bump hält den Rückstand bei null. 0.8.7
# (Sweep 2026-10-22) ebenso: IVFFlat-Build-Overflow + avg ohne Zeilen; live
# gibt es nur HNSW-Indizes. recall_check
# stempelt die pgvector-Version je Lauf (recall.go), ein Versionswechsel ist dort
# als Drift-Marker sichtbar, kein Gate.
# Digest-Pin: der Tag allein floatet die PostgreSQL-Minor mit (18.4 → 18.6 bei
# unverändertem Dockerfile gemessen). Tag bleibt als lesbares Label stehen; beim
# Bump Tag UND Digest gemeinsam ändern
# (`docker buildx imagetools inspect <tag> --format '{{.Manifest.Digest}}'`).
FROM pgvector/pgvector:0.8.7-pg18-trixie@sha256:9d9c930220cb9bf2f956d10a8f909cf9973d9672ca0278aef2c1e6facccad2e0

# TimescaleDB fest gepinnt (Dependency-Sweep 2026-09-19): ohne Versionsangabe zog
# ein Rebuild des unveränderten Dockerfiles 2.28.3 → 2.30.1 (gemessen). TimescaleDB
# belegt shared_preload_libraries — ein Minor-Sprung ist eine bewusste Entscheidung
# mit `ALTER EXTENSION timescaledb UPDATE` als Operator-Schritt, kein Build-Zufall.
# Loader mitgepinnt: das Hauptpaket verlangt ihn nur per `>=`, er würde sonst floaten.
# 2.30.1 verlangt postgresql-18 >= 18.6 — gekoppelt an den Digest der Basis oben.
# 2.31.0 (Sweep 2026-10-22): die zwei umbenannten GUCs (enable_columnarscan,
# direct-compress-Sortierung) sind live nicht gesetzt; der neue Loader braucht
# einen Neustart, den der Image-Tausch ohnehin bringt. Dieselbe Version fährt
# das Integrations-Image timescale/timescaledb-ha:pg18.
ARG TIMESCALEDB_DEB_VERSION=2.31.0~debian13-1806

# Install TimescaleDB from official packagecloud repo (trixie packages for PG18)
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gnupg \
        lsb-release \
    && curl -s https://packagecloud.io/install/repositories/timescale/timescaledb/script.deb.sh | bash \
    && apt-get install -y --no-install-recommends \
        timescaledb-2-postgresql-18=${TIMESCALEDB_DEB_VERSION} \
        timescaledb-2-loader-postgresql-18=${TIMESCALEDB_DEB_VERSION} \
    && apt-get purge -y --auto-remove \
        curl \
        gnupg \
        lsb-release \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# TimescaleDB must be preloaded; pgvector does NOT need preloading
RUN echo "shared_preload_libraries = 'timescaledb'" >> /usr/share/postgresql/postgresql.conf.sample
