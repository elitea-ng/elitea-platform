# Deploying ledger-sync

ledger-sync is deployed with Helm, release name `ledger-sync`, namespace
`ledger`. CI publishes the chart; a deploy is a `helm upgrade` with the new
image tag.

## Rollback

1. Find the last good revision:

       helm history ledger-sync -n ledger

2. Roll back to it:

       helm rollback ledger-sync <REVISION> -n ledger

3. Verify readiness through a port-forward:

       kubectl port-forward -n ledger deploy/ledger-sync 8443:8443
       curl -sk https://localhost:8443/readyz

   Expect HTTP 200 and `{"status":"ready"}`.

4. Watch consumer lag on the dashboard for 10 minutes.

Schema migrations are forward-compatible for one release, so rolling back by
one revision is always safe. Rolling back more than one revision needs a DBA.
