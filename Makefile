.PHONY: build test check install

build:
	GOWORK=off GOTOOLCHAIN=local go build -trimpath -o bin/go-upgrade .
	ln -sf go-upgrade bin/go-upgrade-nds
	ln -sf go-upgrade bin/upgrade-go
	ln -sf go-upgrade bin/upgrade-go-nds

test:
	GOWORK=off GOTOOLCHAIN=local go test -race ./...

check: test
	GOWORK=off GOTOOLCHAIN=local go vet ./...

install: build
	python3 scripts/install.py
