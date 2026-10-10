package runtimegrpc

import (
	"net/http"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
)

// The vector token introspection service (ADR-0031) is served on the
// control listener only when it is composed, and never on the output
// listener.
func TestVectorIntrospectionIsRegisteredOnTheControlListenerOnlyWhenComposed(t *testing.T) {
	certificate, roots := runtimeServerCertificate(t)
	secure, err := NewServerTLSConfig(certificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	config := PrivateServerConfig{
		ControlAddress: ":9443", OutputAddress: ":9444", ContentAddress: ":9445",
		ControlTLS: secure, OutputTLS: secure.Clone(), ContentTLS: secure.Clone(),
		ControlMaxRequestBytes: 64 * 1024, ControlMaxResponseBytes: 80 * 1024,
		OutputMaxRequestBytes: 64 * 1024, OutputMaxResponseBytes: 80 * 1024,
		ControlGRPC: testGRPCServerPolicy(), OutputGRPC: testGRPCServerPolicy(),
		ContentMaxConnections: 16, ContentMaxStreams: 8,
		ContentReadTimeout: 5 * time.Second, ContentWriteTimeout: 30 * time.Second,
		ContentIdleTimeout: 30 * time.Second, ContentMaxHeaderBytes: 8 * 1024,
		ShutdownTimeout: 10 * time.Second,
	}
	services := PrivateServices{
		Control: runtimev1.UnimplementedRuntimeControlServiceServer{},
		Output:  runtimev1.UnimplementedExecutionOutputServiceServer{},
		Content: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}
	const name = "elitea.vector.v1.TokenIntrospectionService"

	without, err := NewPrivateServerSet(config, services)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := without.controlServer.GetServiceInfo()[name]; ok {
		t.Fatal("registered although not composed")
	}

	services.VectorIntrospection = vectorv1.UnimplementedTokenIntrospectionServiceServer{}
	with, err := NewPrivateServerSet(config, services)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := with.controlServer.GetServiceInfo()[name]; !ok {
		t.Fatal("not registered on the control listener")
	}
	if _, ok := with.outputServer.GetServiceInfo()[name]; ok {
		t.Fatal("registered on the output listener")
	}
}
