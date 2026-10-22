module github.com/EliteaAI/elitea-platform/services/elitea-subapp-host

go 1.26.9

replace github.com/EliteaAI/elitea-platform/libs/proto/gen/go => ../../libs/proto/gen/go

require (
	github.com/EliteaAI/elitea-platform/libs/proto/gen/go v0.0.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/nats-io/nats-server/v2 v2.12.6
	google.golang.org/grpc v1.83.2
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.6.0-default-no-op // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/minio/highwayhash v1.0.4-0.20251030100505-070ab1a87a76 // indirect
	github.com/nats-io/jwt/v2 v2.8.1 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

require (
	github.com/EliteaAI/elitea-platform/libs/go/natsconn v0.0.0-00010101000000-000000000000
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/nats-io/nats.go v1.53.1
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.12
)

replace github.com/EliteaAI/elitea-platform/libs/go/natsconn => ../../libs/go/natsconn
