package runtimegrpc

import (
	"net/http"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// The cloud executor's workspace-store service (ADR-0034) is served on the
// control listener when composed, and never on the output listener.
func TestWorkspaceStoreIsRegisteredOnTheControlListenerOnly(t *testing.T) {
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
		Control:   runtimev1.UnimplementedRuntimeControlServiceServer{},
		Output:    runtimev1.UnimplementedExecutionOutputServiceServer{},
		Content:   http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Workspace: runtimev1.UnimplementedWorkspaceStoreServiceServer{},
	}
	const name = "elitea.runtime.v1.WorkspaceStoreService"
	with, err := NewPrivateServerSet(config, services)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := with.controlServer.GetServiceInfo()[name]
	if !ok || len(info.Methods) != 4 {
		t.Fatalf("not registered on the control listener with its four calls: %+v", info)
	}
	if _, ok := with.outputServer.GetServiceInfo()[name]; ok {
		t.Fatal("registered on the output listener")
	}
}

// The egress proxy's audit service (ADR-0034 decision 9) is served on the
// control listener only when composed, and never on the output listener.
func TestEgressAuditIsRegisteredOnTheControlListenerOnlyWhenComposed(t *testing.T) {
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
	const name = "elitea.runtime.v1.EgressAuditService"
	without, err := NewPrivateServerSet(config, services)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := without.controlServer.GetServiceInfo()[name]; ok {
		t.Fatal("registered without being composed")
	}
	services.EgressAudit = runtimev1.UnimplementedEgressAuditServiceServer{}
	with, err := NewPrivateServerSet(config, services)
	if err != nil {
		t.Fatal(err)
	}
	if info, ok := with.controlServer.GetServiceInfo()[name]; !ok || len(info.Methods) != 1 {
		t.Fatalf("not registered on the control listener: %+v", info)
	}
	if _, ok := with.outputServer.GetServiceInfo()[name]; ok {
		t.Fatal("registered on the output listener")
	}
}
