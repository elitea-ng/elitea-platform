import { describe, expect, it } from 'vitest';

import { WEBHOOK_MODES, buildExampleRequest, modeRequest, signatureHeaderFor, webhookModeFromAuthMode, webhookModeFromValue } from './pipelineWebhookModal.lib';

describe('pipelineWebhookModal.lib — the four webhook modes', () => {
  it.each([
    [undefined, undefined, WEBHOOK_MODES.custom],
    ['token', 'custom', WEBHOOK_MODES.custom],
    ['token', 'gitlab', WEBHOOK_MODES.gitlab],
    ['hmac_sha256', 'github', WEBHOOK_MODES.github],
    ['standard_webhooks_hmac', 'gitlab', WEBHOOK_MODES.gitlabSigning],
    ['standard_webhooks_hmac', 'custom', WEBHOOK_MODES.gitlabSigning],
  ])('reads auth_mode %s with provider %s as %s', (authMode, provider, want) => {
    expect(webhookModeFromAuthMode(authMode, provider)).toBe(want);
  });

  /** The create/rotate body is the whole of "apply this mode" — the backend writes it nowhere else. */
  it.each([
    [WEBHOOK_MODES.custom, { type: 'custom' }],
    [WEBHOOK_MODES.github, { type: 'github' }],
    [WEBHOOK_MODES.gitlab, { type: 'gitlab' }],
    [WEBHOOK_MODES.gitlabSigning, { type: 'gitlab', auth_mode: 'standard_webhooks_hmac' }],
  ])('sends %s as %o', (mode, body) => {
    expect(modeRequest(mode)).toEqual(body);
  });

  it('maps an unknown select value to Custom', () => {
    expect(webhookModeFromValue('gitlab_signing')).toBe(WEBHOOK_MODES.gitlabSigning);
    expect(webhookModeFromValue('bitbucket')).toBe(WEBHOOK_MODES.custom);
  });

  it('names the header each signing mode reads, and none for a bearer mode', () => {
    expect(signatureHeaderFor(WEBHOOK_MODES.github, undefined)).toBe('X-Hub-Signature-256');
    expect(signatureHeaderFor(WEBHOOK_MODES.github, 'X-Custom-Sig')).toBe('X-Custom-Sig');
    expect(signatureHeaderFor(WEBHOOK_MODES.gitlabSigning, 'anything')).toBe('webhook-signature');
    expect(signatureHeaderFor(WEBHOOK_MODES.gitlab, undefined)).toBeUndefined();
    expect(signatureHeaderFor(WEBHOOK_MODES.custom, undefined)).toBeUndefined();
  });

  /** A copied example that answers 401 is worse than none: each mode shows the carrier its trigger accepts. */
  it('shows the carrier or signature each mode accepts', () => {
    const url = 'https://elitea.example/api/v2/pipeline_trigger/1/abc/gitlab';
    const gitlab = buildExampleRequest({ url, secret: 's3cret', showSecret: true, mode: WEBHOOK_MODES.gitlab, signatureHeader: undefined });
    expect(gitlab).toContain('X-Gitlab-Token: s3cret');
    expect(gitlab).not.toContain('Authorization');

    const signing = buildExampleRequest({ url, secret: 'whsec_a2V5', showSecret: false, mode: WEBHOOK_MODES.gitlabSigning, signatureHeader: 'webhook-signature' });
    expect(signing).toContain('webhook-signature: v1,');
    expect(signing).toContain('webhook-id');
    expect(signing).toContain('webhook-timestamp');
    expect(signing).not.toContain('whsec_a2V5');

    const bearer = buildExampleRequest({ url, secret: 's', showSecret: true, mode: WEBHOOK_MODES.custom, signatureHeader: undefined });
    expect(bearer).toContain('Authorization: Bearer s');
  });
});
