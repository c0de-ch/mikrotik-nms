.PHONY: dev dev-backend dev-frontend build build-backend build-frontend test lint docker-build docker-up docker-down

# Development
dev: dev-backend dev-frontend

dev-backend:
	cd backend && MIKROTIK_NMS_JWT_SECRET=dev-secret MIKROTIK_NMS_DB_PATH=mikrotik-nms.db go run ./cmd/mikrotik-nms/

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
