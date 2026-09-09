import { describe, expect, it } from 'vitest';

import { generateWebhookSecret } from './webhookHelpers';

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
