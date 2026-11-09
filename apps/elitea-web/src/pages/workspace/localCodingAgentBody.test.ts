import { describe, expect, it } from 'vitest';

import { productName } from '@/shared/brand';

import { localCodingAgentBody } from './useLocalCodingAgent';

describe('localCodingAgentBody', () => {
  // ADR-0052 W4: the shipped prompt names the product through the brand, never a literal.
  it('fills the product name into the shipped instructions and the description', () => {
    const body = localCodingAgentBody({ name: 'model-a', project_id: '7' });
    const version = body.versions?.[0];
    expect(version?.instructions).toContain(`through the ${productName()} desktop app`);
    expect(version?.instructions).not.toContain('{{product}}');
    expect(body.description).toContain(`${productName()} desktop app`);
    expect(version?.llm_settings).toEqual({ model_name: 'model-a', model_project_id: 7 });
  });
});
