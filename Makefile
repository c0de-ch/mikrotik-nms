.PHONY: dev dev-backend dev-backend-poll dev-frontend build build-backend build-frontend test fuzz-flow lint docker-build docker-up docker-down

# Development
dev: dev-backend dev-frontend

# Polls nothing: MIKROTIK_NMS_POLLERS defaults to off, so a checkout started
# against a copied production database shows history and never contacts a
# device. Use dev-backend-poll to reach real (lab) devices.
dev-backend:
	cd backend && MIKROTIK_NMS_JWT_SECRET=dev-secret MIKROTIK_NMS_DB_PATH=mikrotik-nms.db go run ./cmd/mikrotik-nms/

# Opt in to device polling from a checkout, fenced to the subnets you name:
#   MIKROTIK_NMS_ALLOWED_TARGET_CIDRS=192.0.2.0/24 make dev-backend-poll
dev-backend-poll:
	@test -n "$$MIKROTIK_NMS_ALLOWED_TARGET_CIDRS" || { echo "set MIKROTIK_NMS_ALLOWED_TARGET_CIDRS to the subnet(s) of the devices this checkout may poll, e.g. MIKROTIK_NMS_ALLOWED_TARGET_CIDRS=192.0.2.0/24 make dev-backend-poll"; exit 1; }
	cd backend && MIKROTIK_NMS_POLLERS=local MIKROTIK_NMS_JWT_SECRET=dev-secret MIKROTIK_NMS_DB_PATH=mikrotik-nms.db go run ./cmd/mikrotik-nms/

dev-frontend:
	cd frontend && NEXT_PUBLIC_API_URL=http://localhost:8080 NEXT_PUBLIC_WS_URL=ws://localhost:8080 npm run dev

# Build
build: build-backend build-frontend

build-backend:
	cd backend && CGO_ENABLED=0 go build -ldflags="-s -w" -o ../bin/mikrotik-nms ./cmd/mikrotik-nms/

build-frontend:
	cd frontend && npm run build

# Test
test:
	cd backend && go test ./...

test-verbose:
	cd backend && go test -v ./...

# Fuzz the guarded flow decoder (goflow2 v2.2.6 has remote DoS bugs the
# guards neutralise); `go test ./...` only replays the seed corpus.
fuzz-flow:
	cd backend && go test ./internal/flow/decode -run '^$$' -fuzz FuzzDecode -fuzztime=60s

# Lint
lint:
	cd backend && go vet ./...
	cd frontend && npm run lint

# Docker
docker-build:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down

# Clean
clean:
	rm -rf bin/ backend/mikrotik-nms.db frontend/.next
