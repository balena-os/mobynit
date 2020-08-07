module github.com/balena-os/hostapp

go 1.14

require (
	github.com/containerd/containerd v1.3.7 // indirect
	github.com/containerd/continuity v0.0.0-20200710164510-efbc4488d8fe // indirect
	github.com/dchest/uniuri v0.0.0-20200228104902-7aecb25e1fe5
	github.com/docker/distribution v2.7.1+incompatible // indirect
	github.com/docker/docker v0.0.0-00010101000000-000000000000
	github.com/docker/go-connections v0.4.0 // indirect
	github.com/docker/go-units v0.4.0 // indirect
	github.com/gogo/protobuf v1.3.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.0.1 // indirect
	github.com/opencontainers/runc v0.1.1
	github.com/opencontainers/runtime-spec v1.0.2 // indirect
	github.com/opencontainers/selinux v1.6.0
	github.com/vbatts/tar-split v0.11.1 // indirect
	golang.org/x/sys v0.0.0-20200728102440-3e129f6d46b1
	google.golang.org/grpc v1.31.0 // indirect
)

replace (
	// this is v19.03.12
	github.com/docker/docker => github.com/moby/moby v17.12.0-ce-rc1.0.20200618181300-9dc6525e6118+incompatible
	// check vendor.conf in the moby tree and take the version from there
	github.com/opencontainers/runc => github.com/opencontainers/runc v1.0.0-rc10
)

