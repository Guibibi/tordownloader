#!/bin/sh
# tordownloader entrypoint — PUID/PGID support for Unraid file ownership.
#
# Convention: PUID=99 (nobody), PGID=100 (users).
# The script creates or reuses a user/group matching those IDs so that
# downloaded files are owned correctly on the host's shared volume.
set -e

PUID="${PUID:-99}"
PGID="${PGID:-100}"
APP_USER="app"
APP_GROUP="app"

echo "[entrypoint] PUID=${PUID} PGID=${PGID}"

# ── Group ────────────────────────────────────────────────────────────────
if getent group "${PGID}" >/dev/null 2>&1; then
  APP_GROUP="$(getent group "${PGID}" | cut -d: -f1)"
  echo "[entrypoint] using existing group ${APP_GROUP} (gid=${PGID})"
else
  addgroup -g "${PGID}" "${APP_GROUP}"
  echo "[entrypoint] created group ${APP_GROUP} (gid=${PGID})"
fi

# ── User ─────────────────────────────────────────────────────────────────
if getent passwd "${PUID}" >/dev/null 2>&1; then
  APP_USER="$(getent passwd "${PUID}" | cut -d: -f1)"
  echo "[entrypoint] using existing user ${APP_USER} (uid=${PUID})"
  # Ensure the user is a member of the target group.
  addgroup "${APP_USER}" "${APP_GROUP}" 2>/dev/null || true
else
  adduser -u "${PUID}" -G "${APP_GROUP}" -D "${APP_USER}"
  echo "[entrypoint] created user ${APP_USER} (uid=${PUID})"
fi

# ── Permissions ──────────────────────────────────────────────────────────
# Ensure the app can write to its data directory and the download volume.
chown -R "${APP_USER}:${APP_GROUP}" /data 2>/dev/null || true
if [ -d /downloads ]; then
  chown "${APP_USER}:${APP_GROUP}" /downloads 2>/dev/null || true
fi

echo "[entrypoint] starting tordownloader as ${APP_USER}:${APP_GROUP}"
exec su-exec "${APP_USER}" "$@"
