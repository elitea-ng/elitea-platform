import { describe, expect, it } from 'vitest';

import { WEBHOOK_EVENT_CATALOGUE, WEBHOOK_EVENT_TYPES } from './webhookEventCatalogue';

// Pins the mirror against internal/events.Catalogue
// (services/elitea-main/internal/events/publisher.go) — a drift here means
// someone changed one side and not the other.
describe('WEBHOOK_EVENT_CATALOGUE', () => {
  it('mirrors the Go catalogue exactly', () => {
    expect(WEBHOOK_EVENT_TYPES).toEqual([
      'pipeline.run.started',
      'pipeline.run.succeeded',
      'pipeline.run.failed',
      'schedule.fired',
      'agent.version.published',
      'agent.version.unpublished',
      'conversation.created',
      'artifact.uploaded',
      'moderation.request.decided',
    ]);
  });

  it('every entry has a non-empty description', () => {
    for (const entry of WEBHOOK_EVENT_CATALOGUE) {
      expect(entry.description.length).toBeGreaterThan(0);
    }
  });

  // pipeline.run.succeeded/failed were the last two unwired entries (the
  // SSRF-hardening wave's second hardening item); every entry is wired now.
  it('marks every entry as wired', () => {
    const unwired = WEBHOOK_EVENT_CATALOGUE.filter((entry) => !entry.wired).map((entry) => entry.type);
    expect(unwired).toEqual([]);
  });
});
