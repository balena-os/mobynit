DEST ?= .

GO ?= go
GO_LDFLAGS ?=

mobynit:
	$(GO) build -o $(DEST)/$@ -ldflags "$(GO_LDFLAGS)" ./cmd/mobynit

hostapp.test:
	$(GO) test -c

RM ?= rm

.PHONY: clean
clean:
	$(RM) mobynit hostapp.test
