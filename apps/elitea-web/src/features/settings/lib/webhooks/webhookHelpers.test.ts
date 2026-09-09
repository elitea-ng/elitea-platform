import { describe, expect, it } from 'vitest';

import { formatWebhookEvents, generateWebhookSecret, parseWebhookEvents } from './webhookHelpers';

describe('generateWebhookSecret', () => {
  it('produces a URL-safe, non-empty string with no padding characters', () => {
    const secret = generateWebhookSecret();
    expect(secret.length).toBeGreaterThan(0);
    expect(secret).toMatch(/^[A-Za-z0-9_-]+$/);
  });

  it('produces a different value on every call', () => {
    expect(generateWebhookSecret()).not.toBe(generateWebhookSecret());
  });
});

describe('parseWebhookEvents', () => {
  it('splits on commas and trims whitespace', () => {
    expect(parseWebhookEvents('application.created, execution.completed ,execution.failed'))
      .toEqual(['application.created', 'execution.completed', 'execution.failed']);
  });

  it('drops empty entries from trailing commas or blank input', () => {
    expect(parseWebhookEvents('application.created,, ')).toEqual(['application.created']);
    expect(parseWebhookEvents('')).toEqual([]);
    expect(parseWebhookEvents('   ')).toEqual([]);
  });
});

describe('formatWebhookEvents', () => {
  it('joins events with a comma and a space, the inverse of parseWebhookEvents', () => {
    const events = ['application.created', 'execution.completed'];
    expect(formatWebhookEvents(events)).toBe('application.created, execution.completed');
    expect(parseWebhookEvents(formatWebhookEvents(events))).toEqual(events);
  });

  it('renders an empty list as an empty string', () => {
    expect(formatWebhookEvents([])).toBe('');
  });
});
