module github.com/helmetica-framework/aludel-cloudscale

go 1.26.4

// The stdlib fixes govulncheck asks for are in 1.26.6, but the devcontainer ships an
// older toolchain with GOTOOLCHAIN=local, where a `go` directive above the installed one
// is a hard stop. A toolchain line is ignored in that mode and honoured everywhere else,
// so local builds keep working while CI and the release build on 1.26.6.
toolchain go1.26.6

require (
	github.com/cloudscale-ch/cloudscale-go-sdk/v6 v6.0.1
	github.com/google/uuid v1.6.0
	github.com/minio/minio-go/v7 v7.2.1
	github.com/spf13/cobra v1.10.2
	google.golang.org/grpc v1.83.0
	sigs.k8s.io/container-object-storage-interface/proto v0.0.0-20260806173055-cc544691e2ef
)

require (
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/klauspost/cpuid/v2 v2.2.11 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/tinylib/msgp v1.6.1 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/ini.v1 v1.67.2 // indirect
)
