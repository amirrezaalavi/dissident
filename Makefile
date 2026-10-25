GO ?= go
MODULES := viberoxy viberayd viber-console
BIN := bin/dissident

.PHONY: help build dissident dissident-test dissident-vet dissident-race vet test race check clean

help:
	@echo "build            - build dissident, then all legacy modules"
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

clean:
	@for m in $(MODULES); do (cd $$m && $(GO) clean); done
	$(GO) clean
	@rm -rf bin
