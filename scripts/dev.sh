#!/usr/bin/env bash
set -e

# Start backend and frontend concurrently for development
trap 'kill 0' EXIT

echo "Starting MikroTik NMS development servers..."

# Backend. MIKROTIK_NMS_POLLERS is deliberately left at its default (off): a
# dev backend never contacts devices, even with a copied production database.
# To poll real (lab) devices use `make dev-backend-poll` with
# MIKROTIK_NMS_ALLOWED_TARGET_CIDRS set to their subnet(s).
(cd backend && \
  MIKROTIK_NMS_JWT_SECRET=dev-secret \
  MIKROTIK_NMS_DB_PATH=mikrotik-nms.db \
  go run ./cmd/mikrotik-nms/) &

# Frontend
(cd frontend && \
  NEXT_PUBLIC_API_URL=http://localhost:8080 \
  NEXT_PUBLIC_WS_URL=ws://localhost:8080 \
  npm run dev) &

wait
