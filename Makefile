DEST ?= .

GO ?= go
GO_LDFLAGS ?= -extldflags "-static" -s -w
DOCKER_BUILDTAGS ?= no_btrfs no_cri no_devmapper no_zfs exclude_disk_quota exclude_graphdriver_btrfs exclude_graphdriver_devicemapper exclude_graphdriver_zfs
GO_BUILDTAGS ?= netgo osusergo static_build $(DOCKER_BUILDTAGS)

mobynit:
	$(GO) build -x -v -o $(DEST)/$@ \
		-ldflags "$(GO_LDFLAGS)" \
		-tags "$(GO_BUILDTAGS)" \
		./cmd/mobynit

hostapp.test:
	$(GO) test -c -o $(DEST)/$@ \
		-ldflags "$(GO_LDFLAGS)" \
		-tags "$(GO_BUILDTAGS)"


RM ?= rm

.PHONY: clean
clean:
	$(RM) mobynit hostapp.test
