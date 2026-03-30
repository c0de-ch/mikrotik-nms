#!/usr/bin/env bash
set -e

# Start backend and frontend concurrently for development
trap 'kill 0' EXIT

echo "Starting MikroTik NMS development servers..."

# Backend
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
