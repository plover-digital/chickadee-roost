# Keep CI and release builds on the reviewed security baseline.
export GOTOOLCHAIN := go1.26.10
GOVULNCHECK_VERSION := v1.9.0

.PHONY: build test release vulncheck
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -buildvcs=true -mod=readonly -trimpath -o bin/chickadee-web ./cmd/chickadee-web
	CGO_ENABLED=0 go build -buildvcs=true -mod=readonly -trimpath -o bin/chickadee-roost ./cmd/chickadee-roost

test:
	go test -race -mod=readonly ./...
	go test -race -mod=readonly github.com/plover-digital/chickadee/workerapi
	python3 -m unittest discover -s scripts -p 'test_*.py'

# No deployment: artifacts and evidence remain in ignored local directories.
release: build
	python3 scripts/release-check.py metadata

vulncheck: release
	mkdir -p build/tools
	GOBIN="$(CURDIR)/build/tools" go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	CGO_ENABLED=0 python3 scripts/release-check.py scan
