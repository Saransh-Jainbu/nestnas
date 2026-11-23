VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test smoke dist clean desktop desktop-deb
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o nestnas .
test:
	go vet ./... && go test ./...
smoke: build
	./scripts/smoke.sh ./nestnas
	SMOKE_ENCRYPT=1 ./scripts/smoke.sh ./nestnas
# Cross-compile single-file binaries for every major OS
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/nestnas-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/nestnas-linux-arm64 .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/nestnas-macos-arm64 .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/nestnas-macos-amd64 .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/nestnas-windows-amd64.exe .
	cd dist && sha256sum nestnas-* > SHA256SUMS
# Native desktop app (needs libgtk-3-dev libwebkit2gtk-4.0-dev). Separate Go module: see desktop/
desktop:
	cd desktop && go build -tags production -ldflags "-s -w -X main.version=$(VERSION)" -o ../nestnas-desktop .
desktop-deb:
	./desktop/package-deb.sh $(VERSION)
clean:
	rm -rf nestnas dist
