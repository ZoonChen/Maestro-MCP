#!/bin/bash
# 01-harden-maestro-role — brief P3-4 logical guard, applied at FIRST
# initialization of a fresh resident PGDATA (mounted at
# /docker-entrypoint-initdb.d by docker-compose.yaml). The live volume
# got the same statements ad hoc during the P3 restore; this file makes
# a brand-new volume safe by default instead.
#
# What it does:
#   1. Optional break-glass superuser `postgres` (created only when
#      MAESTRO_DBA_PASSWORD is provided) — restore/rebuild operations
#      (DROP/CREATE DATABASE) need a superuser, and after step 2 the
#      application role can no longer self-escalate. Store the password
#      with the other pilot-stack secrets (0600), never in git.
#   2. Strip the application role: NOSUPERUSER NOCREATEDB. PostgreSQL
#      superusers bypass CREATEDB checks, so NOCREATEDB alone would not
#      stop a DROP — the pair is what makes the guard real.
#   3. Revoke PUBLIC connect on the database.
set -euo pipefail

if [ -n "${MAESTRO_DBA_PASSWORD:-}" ]; then
  psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" --set=dba_pw="$MAESTRO_DBA_PASSWORD" <<-'EOSQL'
	CREATE ROLE postgres LOGIN SUPERUSER PASSWORD :'dba_pw';
	EOSQL
else
  echo "01-harden: MAESTRO_DBA_PASSWORD not set — no break-glass superuser created" >&2
fi

psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" <<'EOSQL'
ALTER ROLE current_user NOSUPERUSER NOCREATEDB;
REVOKE ALL ON DATABASE :POSTGRES_DB FROM PUBLIC;
EOSQL

echo "01-harden: application role stripped of SUPERUSER/CREATEDB; PUBLIC connect revoked"
