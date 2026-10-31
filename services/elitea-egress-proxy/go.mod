module github.com/elitea-ng/elitea-platform/services/elitea-egress-proxy

go 1.26.9

require (
	github.com/elitea-ng/elitea-platform/libs/go/egresslib v0.0.0-00010101000000-000000000000
	github.com/elitea-ng/elitea-platform/libs/proto/gen/go v0.0.0
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
)

replace github.com/elitea-ng/elitea-platform/libs/go/egresslib => ../../libs/go/egresslib

replace github.com/elitea-ng/elitea-platform/libs/proto/gen/go => ../../libs/proto/gen/go
