module github.com/balena-os/hostapp

go 1.14

// Obtained from the vendor.conf in github.com/docker/docker v19.03.0
replace github.com/opencontainers/runc => github.com/opencontainers/runc v1.0.0-rc8

require (
	github.com/Microsoft/hcsshim v0.9.2 // indirect
	github.com/containerd/continuity v0.3.0 // indirect
	github.com/docker/distribution v2.8.1+incompatible // indirect
	github.com/docker/docker v17.12.0-ce-rc1.0.20190604184418-5fbc0a16e22c+incompatible // v19.03.0
	github.com/opencontainers/image-spec v1.0.2 // indirect
	github.com/opencontainers/runc v1.1.0 // indirect
	github.com/opencontainers/selinux v1.10.1 // indirect
	github.com/vbatts/tar-split v0.11.2 // indirect
	golang.org/x/net v0.0.0-20211216030914-fe4d6282115f // indirect
	golang.org/x/sys v0.0.0-20220412211240-33da011f77ad
	golang.org/x/text v0.3.7 // indirect
	google.golang.org/genproto v0.0.0-20211208223120-3a66f561d7aa // indirect
	google.golang.org/grpc v1.45.0 // indirect
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b // indirect
)
