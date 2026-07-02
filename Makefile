BINARIES := nb
BINDIR := bin
DISTDIR := dist

# The version stamped into the binary (`nb version`): the nearest tag, or a bare
# commit hash before the first tag. Overridable: `make build VERSION=1.0.0`.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -X github.com/Niloen/nbackup/internal/cli.Version=$(VERSION)

.PHONY: all build test vet clean install man completions

all: build

build:
	@mkdir -p $(BINDIR)
	go build -ldflags "$(LDFLAGS)" -o $(BINDIR)/ ./cmd/...

test:
	go test ./...

vet:
	go vet ./...

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/...

# Man pages, generated from the cobra command tree into dist/man (untracked).
# The release workflow runs this before GoReleaser packs them into archives/packages.
man:
	go run ./internal/tools/mkman $(DISTDIR)/man

# Shell completions, via the binary's own `nb completion` (untracked, for packaging).
completions: build
	@mkdir -p $(DISTDIR)/completions
	$(BINDIR)/nb completion bash > $(DISTDIR)/completions/nb.bash
	$(BINDIR)/nb completion zsh > $(DISTDIR)/completions/_nb
	$(BINDIR)/nb completion fish > $(DISTDIR)/completions/nb.fish

clean:
	rm -rf $(BINDIR) $(DISTDIR)
