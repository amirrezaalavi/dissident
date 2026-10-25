GO ?= go

.PHONY: help build vet test race check clean

help:
	@echo "build  - compile both modules"
	@echo "vet    - run go vet on both modules"
	@echo "test   - run both test suites"
	@echo "race   - run both test suites with the race detector"
	@echo "check  - build + vet + test"

build:
	cd viberoxy && $(GO) build ./...
	cd viberayd && $(GO) build ./...

vet:
	cd viberoxy && $(GO) vet ./...
	cd viberayd && $(GO) vet ./...

test:
	cd viberoxy && $(GO) test ./... -count=1
	cd viberayd && $(GO) test ./... -count=1

race:
	cd viberoxy && $(GO) test -race ./... -count=1
	cd viberayd && $(GO) test -race ./... -count=1

check: build vet test

clean:
	cd viberoxy && $(GO) clean
	cd viberayd && $(GO) clean
