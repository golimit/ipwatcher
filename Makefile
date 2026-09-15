BINARY := ipwatcher
VERSION := 0.2.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt tidy clean run docker

all: fmt vet test build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/ipwatcher

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
	rm -rf dist/

run: build
	./$(BINARY) run

docker:
	docker build -t ipwatcher:$(VERSION) .
