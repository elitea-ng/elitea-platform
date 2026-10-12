module github.com/EliteaAI/elitea-platform/services/elitea-subapp-host

go 1.26.9

replace github.com/EliteaAI/elitea-platform/libs/proto/gen/go => ../../libs/proto/gen/go

require (
	github.com/EliteaAI/elitea-platform/libs/proto/gen/go v0.0.0
	github.com/jackc/pgx/v5 v5.9.2
	google.golang.org/grpc v1.83.2
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
