module github.com/elitea-ng/elitea-platform/services/elitea-subapp-host

go 1.26.9

replace github.com/elitea-ng/elitea-platform/libs/proto/gen/go => ../../libs/proto/gen/go

require (
	github.com/elitea-ng/elitea-platform/libs/proto/gen/go v0.0.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/nats-io/nats-server/v2 v2.15.1
	google.golang.org/grpc v1.84.0
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.8.0-default-no-op // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/klauspost/compress v1.20.1 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/nats-io/jwt/v2 v2.8.2 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/time v0.16.0 // indirect
)

require (
	github.com/elitea-ng/elitea-platform/libs/go/natsconn v0.0.0-00010101000000-000000000000
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/nats-io/nats.go v1.55.0
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260706201446-f0a921348800 // indirect
	google.golang.org/protobuf v1.36.12
)

replace github.com/elitea-ng/elitea-platform/libs/go/natsconn => ../../libs/go/natsconn
