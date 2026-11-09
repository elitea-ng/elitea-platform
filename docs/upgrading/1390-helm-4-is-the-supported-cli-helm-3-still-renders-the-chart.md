---
title: "Helm 4 is the supported CLI; Helm 3 still renders the chart"
issue: 1390
date: 2026-10-22
order: 22
---

**Affects:** anyone who runs `helm install` / `helm upgrade` by hand. Argo CD
installs are not affected.

**What changed.** CI lints, installs (the kind smoke) and publishes the charts
with Helm **4.3.1**, and checks on every pull request that Helm **3.22**
renders the `elitea` chart to the same objects, because Argo CD renders with
the Helm 3 it bundles. Supported: Helm 4.3 or newer for the CLI (recommended),
Helm 3.8 or newer where Argo CD (or another Helm 3 tool) renders the chart.

**What to do with the Helm 4 CLI.**

- `helm upgrade` of a release that Helm 3 installed keeps client-side apply
  (`--server-side=auto` follows the release's previous method); a new install
  uses server-side apply. Nothing to do unless an upgrade reports a
  field-manager conflict, then rerun it with `--force-conflicts`.
- `--atomic` is now `--rollback-on-failure`.
- `--wait` now waits with kstatus (`--wait=watcher`). The first-install rule
  is unchanged: no `--wait` on a FIRST install of the elitea chart (see "One
  chart: NATS bundled").
- `helm push`, `helm pull oci://…` and `helm registry login` are unchanged.
