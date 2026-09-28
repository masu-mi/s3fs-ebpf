module github.com/bbsakura/s3fs-ebpf/daemon

go 1.25.0

require (
	cel.dev/cel-go v0.32.0
	github.com/cilium/ebpf v0.22.0
	github.com/minio/minio-go/v7 v7.3.0
	golang.org/x/sys v0.47.0
)

require (
	cel.dev/expr v0.25.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240826202546-f6391c0de4c7 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
	gopkg.in/ini.v1 v1.67.3 // indirect
)

// Local vendor of cilium/ebpf v0.22.0 with a one-line fix in
// asm/instruction.go for the bpf_addr_space_cast Marshal bug
// (upstream Mov marshaller wipes ins.Offset, corrupting the
// addr_space_cast encoding). See third_party/ebpf/README.md.
// TODO: switch to a fork tag once upstream PR lands.
replace github.com/cilium/ebpf => ./third_party/ebpf
