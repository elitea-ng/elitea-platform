---
title: "LLM gateway: the degraded-mode budget cap is divided by the replica count"
issue: 1403
date: 2026-10-22
order: 27
---

**Affects:** every install that runs more than one LLM gateway replica, and
every install that sets `LLM_BUDGET_EXPECTED_REPLICAS` or
`LLM_BUDGET_NATS_DEGRADED_CAP_USD`, and every client of the realtime route.

**What changed.**

* While NATS is down, each gateway replica now admits only its share of the
  degraded-mode budget cap: `floor(cap / LLM_BUDGET_EXPECTED_REPLICAS)`. The
  cap is `LLM_BUDGET_NATS_DEGRADED_CAP_USD`, else 10% of the hard limit.
  Before, each replica admitted the whole cap, so N replicas could spend
  N × cap.
* **Each request admitted while NATS is down reserves against the share**
  (`LLM_BUDGET_NATS_DEGRADED_RESERVATION_USD`, default `0.10`) until its
  real cost is billed. Before, every request admitted ahead of the first
  bill passed, however many there were. Now one share admits at most
  share ÷ reservation requests at a time; the rest get 402 until a request
  finishes. The fleet total is at most the cap, plus what a request costs
  above its reservation.
* **A share smaller than the reservation admits nothing** while NATS is
  down (402). With the default cap and reservation, a replica admits while
  the hard limit is at least N USD (N is `LLM_BUDGET_EXPECTED_REPLICAS`; the
  chart default is 4, so 4 USD). A smaller budget refuses every request
  during an outage.
* **The per-replica cap shrinks as the replica count grows.** The chart sets
  `LLM_BUDGET_EXPECTED_REPLICAS` to the replica ceiling
  (`llmGateway.autoscaling.maxReplicas`, 4 by default, else `replicaCount`).
  With the defaults each replica gets a quarter of the cap. A cap smaller
  than the replica count, in nano-USD, gives a share of 0: that replica
  refuses every request for the budget until NATS returns.
* A replica that starts during an outage gets no share: it refuses every
  spend request with 503 until NATS returns (this was already so).
* A replica exactly at its share now refuses (before, it admitted one more
  request). In the near-limit state the remaining budget is divided too, so
  a remaining budget below the replica count, in nano-USD, refuses with 402.
* **Realtime sessions:** a live session keeps its reservation until it ends.
  When other spend fills the share, the session's next budget check returns
  402 and the session closes, as for any exhausted budget. A session's own
  reservation never refuses it.
* The gateway refuses to start when `LLM_BUDGET_EXPECTED_REPLICAS` is set to
  anything but digits for a number of 1 or more (`0`, `-1`, `+3`, `007`,
  an empty value, spaces and text are refused). Before, such a value was
  read as 1. Unset it to get 1.

**What to do.**

1. Leave `llmGateway.env.LLM_BUDGET_EXPECTED_REPLICAS` empty. The chart
   derives it. Outside the chart, set it to the most gateway replicas that
   can run.
2. To keep the degraded-mode allowance each replica had before, multiply
   `llmGateway.env.LLM_BUDGET_NATS_DEGRADED_CAP_USD` by the replica ceiling.
   This also raises the fleet total. A value of `0` (10% of the hard limit)
   is now the fleet total, not the total per replica.
3. Set `llmGateway.env.LLM_BUDGET_NATS_DEGRADED_RESERVATION_USD` at or above
   the cost of your largest single request. A larger value admits fewer
   requests at once during an outage; a smaller one lets an expensive
   request pass its reservation. Keep it at or below each replica's share
   (cap ÷ replicas), or the replica refuses every request during an
   outage.
