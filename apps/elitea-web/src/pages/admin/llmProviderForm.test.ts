/**
 * The provider dialog's field model: the vLLM Anthropic switch is sent as a
 * JSON boolean (the gateway decodes it into a Go bool), and the keyless
 * Authentication choice hides the stored key and always states the mode.
 */
import { describe, expect, it } from 'vitest';

import {
  endpointOrigin,
  missingProviderFields,
  providerAuthMode,
  providerDataFor,
  providerFields,
  supportsWorkloadIdentity,
} from './llmProviderForm';

const keys = (fields: ReturnType<typeof providerFields>) => fields.map((field) => field.key);

describe('vLLM use_anthropic_endpoints', () => {
  it('is a boolean field of vllm only', () => {
    const field = providerFields('vllm').find((item) => item.key === 'use_anthropic_endpoints');
    expect(field?.kind).toBe('boolean');
    expect(keys(providerFields('ollama'))).not.toContain('use_anthropic_endpoints');
  });

  it('is sent as a JSON boolean, false when unticked', () => {
    const base = { api_base: 'http://vllm:8000/v1' };
    expect(providerDataFor('vllm', { ...base, use_anthropic_endpoints: 'true' })['use_anthropic_endpoints']).toBe(true);
    expect(providerDataFor('vllm', base)['use_anthropic_endpoints']).toBe(false);
  });

  it('does not make the form incomplete', () => {
    expect(missingProviderFields('vllm', { api_base: 'http://vllm:8000/v1' }, false)).toEqual([]);
  });
});

describe('keyless provider authentication', () => {
  it('is offered for Vertex AI and DIAL only', () => {
    expect(supportsWorkloadIdentity('vertex_ai')).toBe(true);
    expect(supportsWorkloadIdentity('ai_dial')).toBe(true);
    expect(supportsWorkloadIdentity('open_ai')).toBe(false);
  });

  it('reads an absent or unknown mode as key', () => {
    expect(providerAuthMode({})).toBe('key');
    expect(providerAuthMode({ auth_mode: 'other' })).toBe('key');
    expect(providerAuthMode({ auth_mode: 'workload_identity' })).toBe('workload_identity');
  });

  it('hides the Vertex service account and sends none in workload identity mode', () => {
    expect(keys(providerFields('vertex_ai', 'workload_identity'))).not.toContain('vertex_credentials');
    const values = {
      auth_mode: 'workload_identity',
      vertex_project: 'proj',
      vertex_location: 'us-central1',
      vertex_credentials: '{"type":"service_account"}',
    };
    const data = providerDataFor('vertex_ai', values);
    expect(data).toEqual({ vertex_project: 'proj', vertex_location: 'us-central1', auth_mode: 'workload_identity' });
    expect(missingProviderFields('vertex_ai', values, false)).toEqual([]);
  });

  it('replaces the DIAL API key with the optional token audience', () => {
    const fields = keys(providerFields('ai_dial', 'workload_identity'));
    expect(fields).not.toContain('api_key');
    expect(fields).toContain('token_audience');
    const data = providerDataFor('ai_dial', {
      auth_mode: 'workload_identity',
      api_base: 'https://dial.example.com',
      api_key: 'stale',
      token_audience: ' https://aud.example.com ',
    });
    expect(data['api_key']).toBeUndefined();
    expect(data['token_audience']).toBe('https://aud.example.com');
    expect(data['auth_mode']).toBe('workload_identity');
    expect(missingProviderFields('ai_dial', { auth_mode: 'workload_identity', api_base: 'https://d' }, false)).toEqual([]);
  });

  it('sends auth_mode key explicitly, so a switch back is stored', () => {
    const data = providerDataFor('ai_dial', { api_base: 'https://dial.example.com', api_key: 'k' });
    expect(data).toMatchObject({ auth_mode: 'key', api_key: 'k' });
    expect(keys(providerFields('ai_dial'))).not.toContain('token_audience');
  });

  it('sends no auth_mode for a type without the choice', () => {
    expect('auth_mode' in providerDataFor('open_ai', { api_key: 'k', auth_mode: 'workload_identity' })).toBe(false);
  });

  it('derives the token audience placeholder from the endpoint origin', () => {
    expect(endpointOrigin('https://dial.example.com/openai/v1')).toBe('https://dial.example.com');
    expect(endpointOrigin('not a url')).toBe('');
  });
});
