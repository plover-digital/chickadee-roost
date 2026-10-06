.PHONY: build test
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -buildvcs=false -mod=readonly -trimpath -o bin/chickadee-web ./cmd/chickadee-web
	CGO_ENABLED=0 go build -buildvcs=false -mod=readonly -trimpath -o bin/chickadee-roost ./cmd/chickadee-roost

test:
	go test -race -mod=readonly ./...
	python3 -m unittest discover -s scripts -p 'test_*.py'
