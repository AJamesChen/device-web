.PHONY: build build-arm64 test clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o device-web ./cmd/device-web

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o device-web ./cmd/device-web

test:
	go test ./...

clean:
	rm -f device-web
