build:
	CGO_ENABLED=1 go build ./cmd/paisabridge

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $(find . -name "*.go" -not -path "./vendor/*")

.PHONY: build test vet fmt
