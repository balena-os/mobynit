DEST ?= .

GO ?= go
GO_LDFLAGS ?= -extldflags "-static" -s -w
GO_BUILDTAGS ?= netgo osusergo static_build

mobynit:
	$(GO) build -o $(DEST)/$@ \
		-ldflags "$(GO_LDFLAGS)" \
		-tags "$(GO_BUILDTAGS)" \
		./cmd/mobynit

hostapp.test:
	$(GO) test -c -o $(DEST)/$@ \
		-ldflags "$(GO_LDFLAGS)" \
		-tags "$(GO_BUILDTAGS)"

mobynit.test:
	$(GO) test -c -o $(DEST)/$@ \
		-ldflags "$(GO_LDFLAGS)" \
		-tags "$(GO_BUILDTAGS)" \
		./cmd/mobynit

.PHONY: test-unit
test-unit:
	$(GO) test -v -tags "$(GO_BUILDTAGS)" -run "^Test(BuildOverlayOptions|CollectUniqueLayers)" .

.PHONY: test-all
test-all: hostapp.test mobynit.test

RM ?= rm

.PHONY: clean
clean:
	$(RM) -f mobynit hostapp.test mobynit.test
