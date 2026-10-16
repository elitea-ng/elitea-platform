#!/usr/bin/env bash
# ADR-0034 decision 9, Docker backend: the dedicated network a cloud
# executor's workspace run joins when its project has the network on.
#
# The compose stack does NOT create this network and the supervisor never
# joins it: an operator runs this script once per Docker host (root, the
# Docker engine's iptables backend). Without the network the worker refuses a
# networked workspace run (`workspace_run.policy_unenforceable` /
# NoEgressNetwork) instead of falling back to a bridge with internal reach.
# A run with the project's network off uses network mode `none`.
#
# What it enforces, in the DOCKER-USER chain (evaluated before Docker's own
# rules for every forwarded packet from the network's bridge):
#   * DNS only to the network's resolvers (ELITEA_WORKSPACE_EGRESS_DNS; the
#     supervisor sets the same servers as the container's DNS, so Docker's
#     embedded resolver forwards to them); port 53 to anything else is
#     dropped;
#   * no RFC 1918, carrier-grade NAT, link-local (cloud metadata
#     169.254.169.254 included), loopback or "this network" destination, and
#     the operator's extra ranges (the hosts' own subnets, the compose
#     networks the platform runs on: pass them as arguments);
#   * nothing reaches the host itself (an INPUT rule on the bridge: only
#     replies enter), so a service listening on the host is unreachable;
#   * everything else (the public internet) is allowed.
# IPv6 is disabled on the network, so fd00:ec2::254 and the other IPv6
# private ranges are unreachable by construction.
#
# It also creates the PROXY network (host allowlist mode, "Host allowlist:
# the egress proxy"): a run whose project restricts the network to a host
# allowlist joins it instead, and the egress proxy is attached to it at a
# fixed address. The network is `--internal` (no route out, no gateway), and
# a DOCKER-USER chain lets a run reach ONLY the proxy's address on the proxy
# port: not another run, not the host, no DNS (the supervisor gives the run a
# loopback resolver, and Docker's embedded resolver does not forward for an
# internal network). Start the proxy on it with
#   docker network connect --ip "$ELITEA_WORKSPACE_PROXY_ADDRESS" \
#     "$ELITEA_WORKSPACE_PROXY_NETWORK" <proxy container>
# (deploy/docker-compose.egress-proxy.yml does this), and give elitea-main
# ELITEA_EGRESS_PROXY_URL=http://$ELITEA_WORKSPACE_PROXY_ADDRESS:3128.
#
# Usage: workspace-egress.sh [extra-deny-cidr ...]
# Env:   ELITEA_WORKSPACE_EGRESS_NETWORK (default elitea-workspace-egress),
#        ELITEA_WORKSPACE_EGRESS_SUBNET  (default 172.31.250.0/24),
#        ELITEA_WORKSPACE_EGRESS_DNS     (space-separated public resolvers,
#                                         default "1.1.1.1 9.9.9.9"),
#        ELITEA_WORKSPACE_PROXY_NETWORK  (default elitea-workspace-proxy),
#        ELITEA_WORKSPACE_PROXY_SUBNET   (default 172.31.251.0/24),
#        ELITEA_WORKSPACE_PROXY_ADDRESS  (the proxy's address on it,
#                                         default 172.31.251.2),
#        ELITEA_WORKSPACE_PROXY_PORT     (default 3128).
set -euo pipefail

network="${ELITEA_WORKSPACE_EGRESS_NETWORK:-elitea-workspace-egress}"
subnet="${ELITEA_WORKSPACE_EGRESS_SUBNET:-172.31.250.0/24}"
bridge="br-elitea-ws"
read -r -a resolvers <<< "${ELITEA_WORKSPACE_EGRESS_DNS:-1.1.1.1 9.9.9.9}"
deny=(10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16 127.0.0.0/8 0.0.0.0/8 "$@")

if ! docker network inspect "$network" >/dev/null 2>&1; then
  docker network create --driver bridge --subnet "$subnet" --ipv6=false \
    --opt com.docker.network.bridge.name="$bridge" \
    --opt com.docker.network.bridge.enable_icc=false \
    --label sandbox.elitea.ai/kind=workspace-run "$network" >/dev/null
fi

chain=ELITEA-WORKSPACE-EGRESS
iptables -N "$chain" 2>/dev/null || iptables -F "$chain"
# Replies to allowed connections.
iptables -A "$chain" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
# DNS reaches the network's resolvers only.
for resolver in "${resolvers[@]}"; do
  iptables -A "$chain" -d "$resolver" -p udp --dport 53 -j RETURN
  iptables -A "$chain" -d "$resolver" -p tcp --dport 53 -j RETURN
done
iptables -A "$chain" -p udp --dport 53 -j DROP
iptables -A "$chain" -p tcp --dport 53 -j DROP
for cidr in "${deny[@]}"; do
  iptables -A "$chain" -d "$cidr" -j DROP
done
iptables -A "$chain" -j RETURN
iptables -C DOCKER-USER -i "$bridge" -j "$chain" 2>/dev/null \
  || iptables -I DOCKER-USER -i "$bridge" -j "$chain"
# The host itself: DOCKER-USER sees forwarded packets only, so a run could
# still open a connection to a service listening on the host (Main, the
# supervisor, a database) through the bridge's gateway address. Only replies
# enter the host from the bridge.
input_chain=ELITEA-WORKSPACE-HOST
iptables -N "$input_chain" 2>/dev/null || iptables -F "$input_chain"
iptables -A "$input_chain" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -A "$input_chain" -j DROP
iptables -C INPUT -i "$bridge" -j "$input_chain" 2>/dev/null \
  || iptables -I INPUT -i "$bridge" -j "$input_chain"
echo "workspace egress network $network ($subnet) is ready; set it as the supervisor's workspace egress network"

# --- The proxy network (host allowlist mode). ---
proxy_network="${ELITEA_WORKSPACE_PROXY_NETWORK:-elitea-workspace-proxy}"
proxy_subnet="${ELITEA_WORKSPACE_PROXY_SUBNET:-172.31.251.0/24}"
proxy_address="${ELITEA_WORKSPACE_PROXY_ADDRESS:-172.31.251.2}"
proxy_port="${ELITEA_WORKSPACE_PROXY_PORT:-3128}"
proxy_bridge="br-elitea-wsp"
if ! docker network inspect "$proxy_network" >/dev/null 2>&1; then
  # ICC off: Docker drops bridge-local traffic, and the chain below admits
  # the one flow allowed (a run to the proxy). The proxy is outside the
  # range Docker assigns to runs.
  docker network create --driver bridge --internal --subnet "$proxy_subnet" \
    --ip-range "$(printf '%s' "$proxy_subnet" | sed 's#\.0/24$#.128/25#')" --ipv6=false \
    --opt com.docker.network.bridge.name="$proxy_bridge" \
    --opt com.docker.network.bridge.enable_icc=false \
    --label sandbox.elitea.ai/kind=workspace-run "$proxy_network" >/dev/null
fi
proxy_chain=ELITEA-WORKSPACE-PROXY
iptables -N "$proxy_chain" 2>/dev/null || iptables -F "$proxy_chain"
# Replies (the proxy answering a run).
iptables -A "$proxy_chain" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
# A run to the proxy port, and nothing else.
iptables -A "$proxy_chain" -o "$proxy_bridge" -d "$proxy_address" -p tcp --dport "$proxy_port" -j ACCEPT
iptables -A "$proxy_chain" -j DROP
iptables -C DOCKER-USER -i "$proxy_bridge" -j "$proxy_chain" 2>/dev/null \
  || iptables -I DOCKER-USER -i "$proxy_bridge" -j "$proxy_chain"
# The host: nothing but replies.
iptables -C INPUT -i "$proxy_bridge" -j "$input_chain" 2>/dev/null \
  || iptables -I INPUT -i "$proxy_bridge" -j "$input_chain"
echo "workspace proxy network $proxy_network ($proxy_subnet) is ready; attach the egress proxy at $proxy_address and set it as the supervisor's workspace proxy network"
