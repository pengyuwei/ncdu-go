APP_NAME := ncdu-go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DIR := bin
DIST_DIR := dist
LDFLAGS := -s -w
PLATFORMS ?= darwin/amd64 darwin/arm64

.PHONY: all build release clean checksums

all: build

build:
	mkdir -p $(BUILD_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME) .

release: clean
	mkdir -p $(DIST_DIR)
	@set -e; for platform in $(PLATFORMS); do \
		goos=$${platform%/*}; \
		goarch=$${platform#*/}; \
		bin_name=$(APP_NAME); \
		archive_ext=tar.gz; \
		if [ "$$goos" = "windows" ]; then \
			bin_name=$(APP_NAME).exe; \
			archive_ext=zip; \
		fi; \
		workdir=$(DIST_DIR)/$(APP_NAME)_$(VERSION)_$${goos}_$${goarch}; \
		mkdir -p "$$workdir"; \
		echo "Building $$goos/$$goarch"; \
		GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o "$$workdir/$$bin_name" .; \
		if [ "$$archive_ext" = "zip" ]; then \
			(cd $(DIST_DIR) && zip -qr "$(APP_NAME)_$(VERSION)_$${goos}_$${goarch}.zip" "$(APP_NAME)_$(VERSION)_$${goos}_$${goarch}"); \
		else \
			(cd $(DIST_DIR) && tar -czf "$(APP_NAME)_$(VERSION)_$${goos}_$${goarch}.tar.gz" "$(APP_NAME)_$(VERSION)_$${goos}_$${goarch}"); \
		fi; \
		rm -rf "$$workdir"; \
	done
	$(MAKE) checksums

checksums:
	cd $(DIST_DIR) && shasum -a 256 * > checksums.txt

clean:
	rm -rf $(BUILD_DIR) $(DIST_DIR)
