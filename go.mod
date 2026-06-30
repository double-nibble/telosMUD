module github.com/double-nibble/telosmud

go 1.25.7

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/nats-io/nats.go v1.52.0
	github.com/pressly/goose/v3 v3.27.1
	github.com/redis/go-redis/v9 v9.21.0
	github.com/stretchr/testify v1.11.1
	github.com/yuin/gopher-lua v1.1.1
	golang.org/x/tools v0.43.0
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

// TELOSMUD FORK (docs/PHASE7-PLAN.md P7-D6): the Lua runtime is our fork of gopher-lua v1.1.1 —
// github.com/double-nibble/gopher-lua — adding a per-call VM instruction-count abort
// (mainLoopWithContext); upstream has no SetHook/MaskCount. The fork KEEPS the module path
// `github.com/yuin/gopher-lua`, so imports are unchanged and this is a pure source swap via replace.
replace github.com/yuin/gopher-lua => github.com/double-nibble/gopher-lua v0.0.0-20260630140553-87185f489027

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	github.com/sethvargo/go-retry v0.3.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.50.0 // indirect
	golang.org/x/mod v0.34.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260420184626-e10c466a9529 // indirect
)
