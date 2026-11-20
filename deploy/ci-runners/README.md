# eng-runner: the self-hosted CI runner pool

The runner image is described in [`image/README.md`](image/README.md).

## Native stack name map

The pool cannot run containers, so a native stack runs each compose service
as a process on the pod's loopback. The owner's scale-set values give the
runner pod `hostAliases`, and each compose service name gets its own
`127.0.0.x` address. That way:

- every process binds its own address at its compose port, so two services
  that both listen on 9443 (or 8443) never collide;
- the hostnames in configs, seeded rows, mTLS SANs and specs stay exactly as
  compose writes them.

Linux answers the whole of `127.0.0.0/8` on `lo`, so no privilege is needed.

| Address | Names |
|---|---|
| 127.0.0.1 | `elitea-main`, `oidc-mock`, `oidc.localhost` |
| 127.0.0.2 | `postgres` |
| 127.0.0.3 | `nats` |
| 127.0.0.4 | `rustfs` |
| 127.0.0.5 | `elitea-deepwiki` |
| 127.0.0.6 | `elitea-inventory` |
| 127.0.0.7 | `elitea-web` |
| 127.0.0.8 | `pgbouncer` |
| 127.0.0.9 | `qdrant` |
| 127.0.0.10 | `elitea-llm-gateway` |
| 127.0.0.11 | `elitea-scheduler` |
| 127.0.0.12 | `elitea-vector` |
| 127.0.0.13 | `elitea-worker` |
| 127.0.0.14 | `elitea-validate-worker` |
| 127.0.0.15 | `elitea-deepwiki-engine` |
| 127.0.0.16 | `elitea-inventory-engine` |
| 127.0.0.17 | `llm-mock` |
| 127.0.0.18 | `llm-real` |
| 127.0.0.19 | `mcp-mock` |
| 127.0.0.20 | `mcp-mock-trust` |
| 127.0.0.21 | `qtest-mock` |
| 127.0.0.22 | `ado-mock` |
| 127.0.0.23 | `otel-collector` |
| 127.0.0.24 | `openbao` |
| 127.0.0.25 | `azurite` |
| 127.0.0.26 | `fake-gcs-server` |
| 127.0.0.27 | `egress-proxy` |
| 127.0.0.28 | `governed-mocks`, `entra.governed.test`, `jira.governed.test`, `sharepoint.governed.test` |
| 127.0.0.29 | `governed-nats-tap` |

**Why 127.0.0.1 is shared.** `elitea-main` and the OIDC mock are where the
browser goes. Chromium resolves every `*.localhost` name to loopback by
itself, so `oidc.localhost` has to be on 127.0.0.1.

**Deliberately not mapped:** `graph.microsoft.com` and
`login.microsoftonline.com`. The pod's `/etc/hosts` applies to every job on
the pool, so mapping them would break real access to Microsoft for all of
them. A native governed stack has to point its clients at
`governed-mocks` explicitly instead.

**The `services:` jobs do not use the map.** They reach PostgreSQL, NATS,
Qdrant and OpenBao on `127.0.0.1:<port>`, the same as the port mappings of a
`services:` block on a hosted runner.

**The map lives in the owner's values, not here.** The kubelet writes the
pod's `/etc/hosts`, so an image cannot carry it. A new compose service needs
its own address there before a native stack can run it, and this table needs
the same change.
