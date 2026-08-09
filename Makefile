PLUGIN := cpa-key-policy
PKG := ./cmd/cpa-key-policy
DIST := dist
WEB := web
EMBED_INDEX := internal/plugin/web/dist/index.html

.PHONY: test check-version check-model-domain web-build build-linux-amd64 build-linux-arm64 build-linux clean

test:
	go test ./...

check-version:
	hack/check-version.sh

check-model-domain:
	hack/check-model-domain.sh

# Build the single-file web UI and place it where the Go embed expects it.
web-build:
	cd $(WEB) && npm ci && VITE_HOSTED=1 npm run build
	cp $(WEB)/dist/index.html $(EMBED_INDEX)

build-linux-amd64: web-build
	mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -buildvcs=false -tags cshared -buildmode=c-shared -o $(DIST)/$(PLUGIN)_linux_amd64.so $(PKG)

build-linux-arm64: web-build
	mkdir -p $(DIST)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -buildvcs=false -tags cshared -buildmode=c-shared -o $(DIST)/$(PLUGIN)_linux_arm64.so $(PKG)

build-linux: build-linux-amd64 build-linux-arm64

clean:
	rm -rf $(DIST)
