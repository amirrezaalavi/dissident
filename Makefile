GO ?= go
MODULES := viberoxy viberayd viber-console
BIN := bin/dissident

.PHONY: help build dissident dissident-test dissident-vet dissident-race vet test race check clean release

help:
	@echo "build            - build dissident, then all legacy modules"
	@echo "release          - cross-build linux/amd64+arm64 release assets into dist/"
	@echo "dissident        - build the dissident binary at $(BIN)"
	@echo "dissident-test   - run dissident (root module) tests"
	@echo "dissident-vet    - run go vet on dissident"
	@echo "dissident-race   - run dissident tests with the race detector"
	@echo "vet              - vet dissident and all legacy modules"
	@echo "test             - test dissident and all legacy modules"
	@echo "race             - race-test dissident and all legacy modules"
	@echo "check            - build + vet + test"

build: dissident
	@for m in $(MODULES); do echo "== $$m"; (cd $$m && $(GO) build ./...) || exit 1; done

dissident:
	@mkdir -p bin
	$(GO) build -trimpath -o $(BIN) ./cmd/dissident

dissident-test:
	$(GO) test ./... -count=1

dissident-vet:
	$(GO) vet ./...

dissident-race:
	$(GO) test -race ./... -count=1

vet: dissident-vet
	@for m in $(MODULES); do echo "== $$m"; (cd $$m && $(GO) vet ./...) || exit 1; done

test: dissident-test
	@for m in $(MODULES); do echo "== $$m"; (cd $$m && $(GO) test ./... -count=1) || exit 1; done

race: dissident-race
	@for m in $(MODULES); do echo "== $$m"; (cd $$m && $(GO) test -race ./... -count=1) || exit 1; done

check: build vet test

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST := dist

release:
	@rm -rf $(DIST) && mkdir -p $(DIST)
	@for arch in amd64 arm64; do \
		echo "== linux/$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "-s -w -X main.version=$(VERSION)" \
			-o $(DIST)/dissident-linux-$$arch ./cmd/dissident || exit 1; \
		(cd viberayd && CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "-s -w" -o ../$(DIST)/viberayd-linux-$$arch ./cmd/viberayd) || exit 1; \
		(cd viberoxy && CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "-s -w" -o ../$(DIST)/viberoxy-linux-$$arch .) || exit 1; \
		(cd viber-console && CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "-s -w" -o ../$(DIST)/viber-console-linux-$$arch ./cmd/console) || exit 1; \
	done
	@cd $(DIST) && sha256sum * > SHA256SUMS
	@ls -lh $(DIST)

clean:
	@for m in $(MODULES); do (cd $$m && $(GO) clean); done
	$(GO) clean
	@rm -rf bin $(DIST)
