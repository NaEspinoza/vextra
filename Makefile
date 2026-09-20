VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-demo)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
PREFIX  ?= /usr/local

.PHONY: build test vet dist install demo clean

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o vextra .

vet:
	go vet ./...

test:
	go test ./...

# go test -race requiere CGO (gcc); útil en desarrollo.
test-race:
	CGO_ENABLED=1 go test -race ./...

# Binarios estáticos para Linux: amd64, arm64 y armv7 (Raspberry Pi, NAS...).
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/vextra-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/vextra-linux-arm64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/vextra-linux-armv7 .
	ls -lh dist/

install: build
	PREFIX=$(PREFIX) ./install.sh ./vextra

demo: build
	./scripts/demo.sh

clean:
	rm -rf vextra dist
