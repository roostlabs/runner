# Build and package the Runner.
#
# The binary has to stay a single static file an installer can drop onto a box,
# so cgo is off everywhere. Both release targets are Linux: that is what the
# installer supports.

BIN := roost-runner
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
DIST := dist
PLATFORMS := linux/amd64 linux/arm64

export CGO_ENABLED := 0

.PHONY: build check test dist clean lint-install

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/runner

check:
	gofmt -l .
	go vet ./...
	go test -race ./...

test:
	go test ./...

# dist builds what install.sh downloads: one tarball per platform, each with a
# checksum beside it. The installer refuses a mismatch and warns when a checksum
# is missing, so publishing these is not optional.
dist: clean
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o $(DIST)/$(BIN) ./cmd/runner || exit 1; \
		tar -czf $(DIST)/$(BIN)_$${os}_$${arch}.tar.gz -C $(DIST) $(BIN) || exit 1; \
		rm -f $(DIST)/$(BIN); \
		( cd $(DIST) && \
		  if command -v sha256sum >/dev/null; then \
			sha256sum $(BIN)_$${os}_$${arch}.tar.gz > $(BIN)_$${os}_$${arch}.tar.gz.sha256; \
		  else \
			shasum -a 256 $(BIN)_$${os}_$${arch}.tar.gz > $(BIN)_$${os}_$${arch}.tar.gz.sha256; \
		  fi ) || exit 1; \
	done
	@ls -1 $(DIST)

# lint-install runs shellcheck in a container, so no host install is needed.
lint-install:
	docker run --rm -v '$(CURDIR):/mnt:ro' -w /mnt koalaman/shellcheck:stable install.sh

clean:
	rm -rf $(DIST) $(BIN)
