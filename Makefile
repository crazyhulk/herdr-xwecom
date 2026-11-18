.PHONY: build test lint
build:
	sh scripts/build.sh
test:
	go test -race ./...
lint:
	gofmt -l . | (! grep .) && go vet ./...
