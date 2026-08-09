PLUGIN := cpa-key-policy
PKG := ./cmd/cpa-key-policy
DIST := dist
WEB := web
EMBED_INDEX := internal/plugin/web/dist/index.html

.PHONY: test check-model-domain-v3 web-build build-linux-amd64 build-linux-arm64 build-linux build-migrator-linux-amd64 check-migrator-linux-amd64 clean

test:
	go test ./...

check-model-domain-v3:
	hack/check-model-domain-v3.sh

# Build the single-file web UI and place it where the Go embed expects it.
web-build:
	cd $(WEB) && npm install && VITE_HOSTED=1 npm run build
	cp $(WEB)/dist/index.html $(EMBED_INDEX)

build-linux-amd64: web-build
	mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -buildvcs=false -tags cshared -buildmode=c-shared -o $(DIST)/$(PLUGIN)_linux_amd64.so $(PKG)

build-linux-arm64: web-build
	mkdir -p $(DIST)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 go build -buildvcs=false -tags cshared -buildmode=c-shared -o $(DIST)/$(PLUGIN)_linux_arm64.so $(PKG)

build-linux: build-linux-amd64 build-linux-arm64

build-migrator-linux-amd64:
	mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -trimpath -o $(DIST)/migrate-model-schema_linux_amd64 ./cmd/migrate-model-schema
	file $(DIST)/migrate-model-schema_linux_amd64 | grep -Eq 'ELF 64-bit.*x86-64'
	sha256sum $(DIST)/migrate-model-schema_linux_amd64 > $(DIST)/migrate-model-schema_linux_amd64.sha256

check-migrator-linux-amd64: build-migrator-linux-amd64
	sha256sum -c $(DIST)/migrate-model-schema_linux_amd64.sha256

clean:
	rm -rf $(DIST)
