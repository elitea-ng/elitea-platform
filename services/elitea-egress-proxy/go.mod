module github.com/EliteaAI/elitea-platform/services/elitea-egress-proxy

go 1.26.9

require (
	github.com/EliteaAI/elitea-platform/libs/go/egresslib v0.0.0-00010101000000-000000000000
	github.com/EliteaAI/elitea-platform/libs/proto/gen/go v0.0.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)

replace github.com/EliteaAI/elitea-platform/libs/go/egresslib => ../../libs/go/egresslib

replace github.com/EliteaAI/elitea-platform/libs/proto/gen/go => ../../libs/proto/gen/go
