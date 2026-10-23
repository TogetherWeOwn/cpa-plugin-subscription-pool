VERSION ?= 0.6.0
GO ?= go
PLUGIN_ID = subscription-pool
OUT ?= dist/$(PLUGIN_ID)-v$(VERSION).so
ARCHIVE ?= dist/$(PLUGIN_ID)_$(VERSION)_linux_amd64.zip
OPERATOR ?= dist/$(PLUGIN_ID)-v$(VERSION)-operator.zip
CHECKSUMS ?= dist/checksums.txt
RELEASE_SHA ?= $(shell git rev-parse HEAD)
COMPATIBILITY_EVIDENCE ?= .github/host-images.json
DEPLOY_DIR ?= ../../plugins/linux/amd64

.PHONY: fmt-check vet lint test test-release validate-source scan-secrets validate-release build-host-integration test-host-image test-host-matrix build package deploy clean

fmt-check:
	test -z "$$(gofmt -l .)"

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

test:
	$(GO) test ./...

test-release:
	$(GO) test ./.github/scripts/release-validation
	.github/scripts/select-release-tag_test.sh

validate-source:
	$(GO) run -buildvcs=false ./.github/scripts/release-validation -mode source

scan-secrets:
	.github/scripts/gitleaks-scan.sh

validate-release:
	test -n "$(TAG)"
	$(GO) run -buildvcs=false ./.github/scripts/release-validation \
		-mode release -tag "$(TAG)" -version "$(VERSION)"

build-host-integration:
	$(GO) build -buildvcs=false -o .github/scripts/host-integration/host-integration ./.github/scripts/host-integration

test-host-image: build-host-integration
	test -n "$(HOST_BINARY)"
	.github/scripts/host-integration/host-integration \
		-host-binary "$(HOST_BINARY)" -plugin "$(OUT)" -image "$(HOST_IMAGE)"

test-host-matrix: build-host-integration
	test -n "$(HOST_IMAGES)"
	.github/scripts/run-host-matrix.sh "$(OUT)" "$(HOST_IMAGES)" "$(HOST_MATRIX_WORK)"

build:
	test "$$($(GO) env GOOS)" = "linux"
	test "$$($(GO) env GOARCH)" = "amd64"
	mkdir -p "$(dir $(OUT))"
	CGO_ENABLED=1 $(GO) build -buildvcs=false -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X main.pluginVersion=$(VERSION)" -o "$(OUT)" ./src
	rm -f dist/*.h
	test -s "$(OUT)"

package: build
	mkdir -p "$(dir $(OPERATOR))"
	install -m 0644 "$(COMPATIBILITY_EVIDENCE)" "$(dir $(OPERATOR))compatibility-evidence.json"
	install -m 0644 deploy/config.yaml.tmpl "$(dir $(OPERATOR))config.yaml.tmpl"
	install -m 0644 registry.json "$(dir $(OPERATOR))registry.json"
	install -m 0644 deploy/router-capacity-source.json "$(dir $(OPERATOR))router-capacity-source.json"
	install -m 0755 deploy/verify-live.sh "$(dir $(OPERATOR))verify-live.sh"
	install -m 0755 deploy/verify-live-opencodego.sh "$(dir $(OPERATOR))verify-live-opencodego.sh"
	install -m 0755 deploy/collector-zai.py "$(dir $(OPERATOR))collector-zai.py"
	install -m 0755 deploy/collector-opencodego.py "$(dir $(OPERATOR))collector-opencodego.py"
	install -m 0755 deploy/prepare-usage-dir.py "$(dir $(OPERATOR))prepare-usage-dir.py"
	install -m 0755 deploy/remove-usage-output.py "$(dir $(OPERATOR))remove-usage-output.py"
	install -m 0755 deploy/rollback.sh "$(dir $(OPERATOR))rollback.sh"
	install -m 0644 deploy/README.md "$(dir $(OPERATOR))README.md"
	printf '%s\n' "$(RELEASE_SHA)" > "$(dir $(OPERATOR))release-sha.txt"
	printf '%s\n' \
		"compatibility-evidence.json=$(dir $(OPERATOR))compatibility-evidence.json" \
		"config.yaml.tmpl=$(dir $(OPERATOR))config.yaml.tmpl" \
		"registry.json=$(dir $(OPERATOR))registry.json" \
		"router-capacity-source.json=$(dir $(OPERATOR))router-capacity-source.json" \
		"verify-live.sh=$(dir $(OPERATOR))verify-live.sh" \
		"verify-live-opencodego.sh=$(dir $(OPERATOR))verify-live-opencodego.sh" \
		"collector-zai.py=$(dir $(OPERATOR))collector-zai.py" \
		"collector-opencodego.py=$(dir $(OPERATOR))collector-opencodego.py" \
		"prepare-usage-dir.py=$(dir $(OPERATOR))prepare-usage-dir.py" \
		"remove-usage-output.py=$(dir $(OPERATOR))remove-usage-output.py" \
		"rollback.sh=$(dir $(OPERATOR))rollback.sh" \
		"README.md=$(dir $(OPERATOR))README.md" \
		"release-sha.txt=$(dir $(OPERATOR))release-sha.txt" \
		> "$(dir $(OPERATOR))operator-manifest.txt"
	cp "$(dir $(OPERATOR))operator-manifest.txt" "$(dir $(OPERATOR))checksum-manifest.txt"
	$(GO) run -buildvcs=false ./.github/scripts/package-release.go \
		-library "$(OUT)" -entry "$(PLUGIN_ID).so" \
		-archive "$(ARCHIVE)" -operator "$(OPERATOR)" \
		-operator-manifest "$(dir $(OPERATOR))operator-manifest.txt" \
		-checksum "$(CHECKSUMS)" \
		-checksum-manifest "$(dir $(OPERATOR))checksum-manifest.txt"
	rm -f "$(dir $(OPERATOR))operator-manifest.txt" "$(dir $(OPERATOR))checksum-manifest.txt"
	test -s "$(ARCHIVE)"
	test -s "$(OPERATOR)"
	test -s "$(CHECKSUMS)"

deploy: build
	mkdir -p "$(DEPLOY_DIR)"
	cp "$(OUT)" "$(DEPLOY_DIR)/"
	test -s "$(DEPLOY_DIR)/$(notdir $(OUT))"

clean:
	rm -rf dist
