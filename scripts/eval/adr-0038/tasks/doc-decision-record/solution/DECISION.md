# ADR: Event bus for the audit pipeline

- Date: 2026-10-14 (architecture sync)
- Participants: Ines Moreau (chair), Tomasz Wilk, Aiko Tanaka, Ravi Iyer

## Status

Accepted

## Context

Every service emits audit events. The new audit pipeline must store them
durably and re-process them: compliance requires replaying the last 30 days of
events whenever the enrichment logic changes. Peak volume is about 8k events
per second today and is projected to reach 20k events per second by the end of
next year. Many enterprise customers run the platform on-prem, several of them
fully air-gapped, so the bus must ship with every install.

## Decision

Use NATS JetStream for the audit event bus, with:

- file storage,
- three replicas,
- a 30-day max age on the audit stream, which provides the replay window.

NATS already runs in every install (request/reply and the command bus),
including air-gapped ones, and the team knows how to operate it. If sustained
throughput ever exceeds 50k events per second, the decision will be revisited
in favour of Kafka.

## Alternatives considered

- **AWS SQS + SNS**: rejected. It is an AWS managed service, and on-prem and
  air-gapped customers cannot depend on it.
- **RabbitMQ**: rejected. Classic queues delete messages once consumed, so
  there is no 30-day replay. Its streams feature might work, but nobody on the
  team has run it, so it would mean learning a new system anyway.
- **Kafka**: rejected. It fits replay and throughput well, but operating it is
  too costly: a dedicated cluster of at least three KRaft brokers, no
  production Kafka experience on the team, an estimated half an engineer
  permanently to keep it healthy, and it would also have to be shipped and
  supported in on-prem customer environments.

## Consequences

- Consumers must be idempotent, because replay redelivers events they have
  already processed.
- Storage is significant: 30 days at 20k events per second is roughly 1.2 TB
  per replica at the current average event size.
- Throughput at the high end is unproven on the three-node cluster until the
  benchmark is done; the schema is not committed before that.
- No new system to operate or ship to customers.

## Follow-ups

| Follow-up | Owner | Due |
|-----------|-------|-----|
| Benchmark 20k events/s sustained with three replicas and post results | Aiko Tanaka | 2026-10-28 |
| Write the stream config and retention policy; document storage sizing | Tomasz Wilk | — |
| Write up this ADR | Ines Moreau | — |
