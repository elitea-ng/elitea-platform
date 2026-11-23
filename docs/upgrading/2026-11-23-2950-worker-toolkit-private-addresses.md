# Worker toolkits and remote MCP refuse private addresses unless permitted (#2950, ADR-0044)

**What changed.** The worker now applies its address check to every toolkit
client, OpenAPI tool, index run and remote MCP session, not only to the web
fetch. Each address a host resolves to is classified, and the worker connects
only to the address it checked. It refuses these addresses, and IP-literal URLs
in them:

- loopback, RFC 1918 and unique-local (`fc00::/7`) addresses;
- CGNAT (`100.64.0.0/10`) and multicast addresses;
- link-local addresses and the cloud metadata endpoints.

A refused request fails the tool call with `egress_host_not_allowed`. The run's
egress summary lists it with the reason `not_public`. The worker logs the
refused host and port (never the URL) once per run, as the event
`egress_not_public_refused`.

The admin console's **Configuration › Worker Egress** section has a new field,
**Private Permit** (`worker_egress.private_permit`). It is the list of private
destinations that toolkits may reach anyway, in both modes, in the allowlist
grammar:

- a host name, `host:port` or `*.domain` admits the private addresses it
  resolves to;
- a CIDR block or IP literal admits the addresses inside it.

The deployment can set the same list as the chart value
`main.env.ELITEA_WORKER_EGRESS_PRIVATE_PERMIT` (comma separated). Main merges
the two lists. Link-local addresses and the metadata endpoints are never
admitted, even when the permit names them. Only these two operator settings
supply the permit. A project setting or a toolkit's URL never adds to it. The permit travels on the
signed worker command (`WorkerEgressPolicyV1.private_allow`). A command
carries an egress policy in `open` mode only when the permit is not empty.

**Who is affected.** Deployments whose toolkits, OpenAPI tools, index sources
or remote MCP servers point at hosts on a private network: a self-hosted Jira,
Confluence, GitLab, Bitbucket, Elasticsearch, SonarQube, a Kubernetes API
server, or an in-cluster MCP service. Until those hosts are permitted, their
tool calls fail with `egress_host_not_allowed` after the upgrade. Deployments
whose toolkits reach only public SaaS hosts are not affected.

**What to do.**

1. Before you upgrade, list the private hosts your toolkits use. The
   **Worker egress hosts** panel under the same section lists the hosts that
   runs reached. Any private host in it needs a permit entry.
2. Add those hosts (or their CIDR blocks) to the permit before you upgrade,
   in the same release: set `main.env.ELITEA_WORKER_EGRESS_PRIVATE_PERMIT`,
   for example `jira.corp.example,10.20.0.0/16`. A malformed entry stops Main
   at start, naming the entry. After the upgrade you can keep the list there
   or move it to **Private Permit** in the console; each Main replica reads
   the console section again within 5 seconds.
3. Upgrade Main and the workers together, as one chart release does. While
   the rollout overlaps, a worker from the previous release refuses a command
   whose policy carries a permit (an unknown field), and that run fails as an
   incompatible command. Upgrade during a quiet period if you set a permit.
4. After the upgrade, check the panel for refused hosts with the reason
   `not_public`. Each one is a private destination that a toolkit tried to
   reach and the permit does not name.
