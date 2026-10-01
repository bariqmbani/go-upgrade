.PHONY: build test check integration integration-race install

build:
	GOWORK=off GOTOOLCHAIN=local go build -trimpath -o bin/go-upgrade .
	ln -sf go-upgrade bin/go-upgrade-nds
	ln -sf go-upgrade bin/upgrade-go
	ln -sf go-upgrade bin/upgrade-go-nds

test:
	GOWORK=off GOTOOLCHAIN=local go test -race ./...

check: test
	GOWORK=off GOTOOLCHAIN=local go vet ./...

integration: build
	python3 tests/run.py

integration-race:
	GOWORK=off GOTOOLCHAIN=local go build -race -o bin/go-upgrade-race .
	UPGRADE_GO_TEST_BINARY="$(CURDIR)/bin/go-upgrade-race" GORACE=atexit_sleep_ms=0 python3 tests/run.py

install: build
	python3 scripts/install.py
