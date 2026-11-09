---
title: "Run finished notifications and web push (#1437, ADR-0045 decisions 5 and 6)"
issue: 1437
date: 2026-11-03
adr: ADR-0045
order: 67
---

New shared migration `0223_web_push`. It adds the table
`centry.push_subscriptions`, the tables `elitea_runtime.web_push_queue` and
`elitea_runtime.web_push_deliveries`, and an `AFTER INSERT` trigger
`web_push_enqueue` on `centry.notifications`. The migration only adds
objects. The trigger queues new `run_finished` and `agent_approval_required`
rows of users with a subscription, so Main does not send old notifications.
Every replica of Main sends; each claims different deliveries.

New notification type `run_finished`. Main writes one row when a chain
ends, or when a schedule, webhook or handed-off cloud run settles. A
cancelled run and a short completed run without a continuation get no row.

New Configuration section **Notifications** with the switch `web_push`
(default on). Client contract 1.32: the operations `getPushConfig`,
`createPushSubscription` and `deletePushSubscription`.

**VAPID key.** At first boot, Main makes one P-256 key for the install. Main
keeps it in the global vault as the hidden secret
`web_push_vapid_private_key`. Do not copy this secret to a different install.
Do not delete it: a new key stops all stored subscriptions. Without a vault,
web push is off and Main starts as usual.

**Contact.** Set `ELITEA_WEB_PUSH_SUBJECT` to a `mailto:` address or an
`https:` URL. When it is empty, Main uses `PUBLIC_BASE_URL` if it is `https`.
With neither, web push is off.

**Egress.** Main sends to the push service of each browser. Allow HTTPS to
these hosts: `fcm.googleapis.com`, `updates.push.services.mozilla.com`,
`web.push.apple.com` and `*.notify.windows.com`. Main uses `HTTPS_PROXY`
and `NO_PROXY` when you set them. Without a proxy, Main connects directly and
refuses private and reserved addresses. When Main cannot reach a push
service, it tries again for at most 24 hours, then drops the send. When a
push service refuses the key (401 or 403), Main drops that send without a
retry; after three refusals in a row a subscription is removed, and that
browser must turn web push on again. The bell is not affected.
