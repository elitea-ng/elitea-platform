/**
 * The cloud classifier form adds one detector to the detectors JSON of a
 * guardrail entry (ADR-0043 decision 1). The detector object must have the
 * keys the gateway reads (internal/guardrail/adapter_*.go).
 */
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';

import { ThemeProvider } from '@mui/material/styles';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { GuardrailCloudFields } from './GovernanceGuardrailCloudFields';
import { buildCloudDetector, type CloudDetectorForm } from './governanceGuardrailCloudDetector';
import { EMPTY_DRAFT, type GovernanceDraft } from './useGatewayGovernancePage';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const BASE: CloudDetectorForm = {
  type: 'azure_content_safety',
  name: '',
  action: 'block',
  endpoint: 'https://cs.example.com',
  apiKeySecret: '{{secret.AZURE_CS_KEY}}',
  outputType: 'FourSeverityLevels',
  thresholds: { Hate: '4', Sexual: '', Violence: '6', SelfHarm: '' },
  blocklists: 'codenames, , terms',
  promptShields: true,
  region: '',
  guardrailIdentifier: '',
  guardrailVersion: '',
  accessKeyIdSecret: '',
  secretAccessKeySecret: '',
  sessionTokenSecret: '',
  template: '',
  authMode: 'key',
  credentialsSecret: '',
};

describe('buildCloudDetector', () => {
  it('builds an azure_content_safety detector', () => {
    expect(buildCloudDetector(BASE)).toEqual({
      type: 'azure_content_safety',
      action: 'block',
      endpoint: 'https://cs.example.com',
      api_key_secret: '{{secret.AZURE_CS_KEY}}',
      categories: { Hate: 4, Violence: 6 },
      blocklists: ['codenames', 'terms'],
      prompt_shields: true,
    });
  });

  it('builds a bedrock_guardrails detector and omits empty fields', () => {
    expect(
      buildCloudDetector({
        ...BASE,
        type: 'bedrock_guardrails',
        name: 'bedrock',
        endpoint: '',
        region: 'us-east-1',
        guardrailIdentifier: 'abc123',
        guardrailVersion: 'DRAFT',
        accessKeyIdSecret: '{{secret.AWS_KEY_ID}}',
        secretAccessKeySecret: '{{secret.AWS_SECRET}}',
      }),
    ).toEqual({
      type: 'bedrock_guardrails',
      action: 'block',
      name: 'bedrock',
      region: 'us-east-1',
      guardrail_identifier: 'abc123',
      guardrail_version: 'DRAFT',
      access_key_id_secret: '{{secret.AWS_KEY_ID}}',
      secret_access_key_secret: '{{secret.AWS_SECRET}}',
    });
  });

  it('builds a keyless model_armor detector without a credentials secret', () => {
    expect(
      buildCloudDetector({
        ...BASE,
        type: 'model_armor',
        action: 'flag',
        endpoint: '',
        template: 'projects/p/locations/us-central1/templates/t',
        authMode: 'workload_identity',
        credentialsSecret: '{{secret.LEFT_OVER}}',
      }),
    ).toEqual({
      type: 'model_armor',
      action: 'flag',
      template: 'projects/p/locations/us-central1/templates/t',
      auth_mode: 'workload_identity',
    });
  });
});

describe('GuardrailCloudFields', () => {
  it('appends the detector to the detectors JSON', () => {
    const onChange = vi.fn<(patch: Partial<GovernanceDraft>) => void>();
    render(
      <ThemeProvider theme={theme}>
        <GuardrailCloudFields draft={{ ...EMPTY_DRAFT, type: 'guardrail' }} onChange={onChange} />
      </ThemeProvider>,
    );
    fireEvent.change(screen.getByTestId('governance-cloud-endpoint'), {
      target: { value: 'https://cs.example.com' },
    });
    fireEvent.change(screen.getByTestId('governance-cloud-azure-key'), {
      target: { value: '{{secret.K}}' },
    });
    fireEvent.click(screen.getByTestId('governance-cloud-add'));
    expect(onChange).toHaveBeenCalledTimes(1);
    const patch = onChange.mock.calls[0]?.[0] as { guardrailDetectors: string };
    const list = JSON.parse(patch.guardrailDetectors) as Record<string, unknown>[];
    expect(list).toHaveLength(2);
    expect(list[0]).toEqual({ type: 'secrets', action: 'mask' });
    expect(list[1]).toMatchObject({
      type: 'azure_content_safety',
      api_key_secret: '{{secret.K}}',
      prompt_shields: true,
    });
  });

  it('offers the warn action and writes it to the detector', () => {
    const onChange = vi.fn<(patch: Partial<GovernanceDraft>) => void>();
    render(
      <ThemeProvider theme={theme}>
        <GuardrailCloudFields draft={{ ...EMPTY_DRAFT, type: 'guardrail' }} onChange={onChange} />
      </ThemeProvider>,
    );
    fireEvent.mouseDown(screen.getByRole('combobox', { name: 'Action' }));
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual(['Block', 'Mask', 'Flag', 'Warn']);
    fireEvent.click(screen.getByRole('option', { name: 'Warn' }));
    fireEvent.change(screen.getByTestId('governance-cloud-endpoint'), { target: { value: 'https://cs.example.com' } });
    fireEvent.change(screen.getByTestId('governance-cloud-azure-key'), { target: { value: '{{secret.K}}' } });
    fireEvent.click(screen.getByTestId('governance-cloud-add'));
    const patch = onChange.mock.calls[0]?.[0] as { guardrailDetectors: string };
    const list = JSON.parse(patch.guardrailDetectors) as Record<string, unknown>[];
    expect(list[1]).toMatchObject({ type: 'azure_content_safety', action: 'warn' });
  });

  it('does not change invalid detectors JSON', () => {
    const onChange = vi.fn<(patch: Partial<GovernanceDraft>) => void>();
    render(
      <ThemeProvider theme={theme}>
        <GuardrailCloudFields
          draft={{
            ...EMPTY_DRAFT,
            type: 'guardrail',
            guardrailDetectors: '[{',
          }}
          onChange={onChange}
        />
      </ThemeProvider>,
    );
    fireEvent.click(screen.getByTestId('governance-cloud-add'));
    expect(onChange).not.toHaveBeenCalled();
    expect(screen.getByText('The detectors must be valid JSON.')).toBeInTheDocument();
  });
});
