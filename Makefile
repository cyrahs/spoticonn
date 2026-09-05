.PHONY: build test web dev image clean

build: web
	go build -trimpath -o bin/spoticonn ./cmd/spoticonn

web:
	cd web && npm ci && npm run build

test:
	go test -race ./...
	go vet ./...
	cd web && npm ci && npm test && npm run build

dev:
	go run ./cmd/spoticonn

image:
	docker build -t spoticonn:0.1.0 .

clean:
	rm -rf bin web/dist web/tsconfig.tsbuildinfo
