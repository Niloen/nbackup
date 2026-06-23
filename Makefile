BINARIES := nb
BINDIR := bin

.PHONY: all build test vet clean install

all: build

build:
	@mkdir -p $(BINDIR)
	go build -o $(BINDIR)/ ./cmd/...

test:
	go test ./...

vet:
	go vet ./...

install:
	go install ./cmd/...

clean:
	rm -rf $(BINDIR)
