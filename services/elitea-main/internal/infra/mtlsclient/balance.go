package mtlsclient

import "google.golang.org/grpc"

// RoundRobinServiceConfig is the gRPC service configuration that spreads the
// calls of one client connection over every address its resolver returns.
const RoundRobinServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

// Balanced is the dial option every internal hop of elitea-main uses to reach
// a service that runs more than one replica: the DeepWiki and Inventory hosts
// (providerhost/proxy, providerhost/platformops) and elitea-vector
// (infra/vectorclient).
//
// WHY. A gRPC client keeps ONE long-lived HTTP/2 connection per address and
// sends every call over it. The default policy (pick_first) uses the first
// address only. Behind a ClusterIP Service that address is the virtual IP,
// and kube-proxy picks one pod per CONNECTION, so every call of this
// elitea-main replica goes to that one pod until the connection closes. A
// scaled-out replica then receives no calls.
//
// With round_robin over a HEADLESS Service, the DNS resolver returns one
// address per ready pod and the client opens a subchannel to each one. The
// calls go to the pods in turn. The servers set MaxConnectionAge, so the
// client re-resolves at least that often and finds a new pod. A pod that is
// not ready leaves the DNS answer and receives no new calls.
//
// Behind a ClusterIP Service (one address) round_robin is the same as
// pick_first, so the option is safe for every target.
func Balanced() grpc.DialOption {
	return grpc.WithDefaultServiceConfig(RoundRobinServiceConfig)
}
