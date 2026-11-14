/**
 * Issue 2012: the "Credentials / Personal data" preset makes the matching
 * guardrail entries through the ordinary create route, one per stage, and
 * keeps the screen open with the server's reason when a create fails.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';

import { GovernanceGuardrailPresetDialog } from './GovernanceGuardrailPresetDialog';
import { DEFAULT_PRESET, presetEntries, presetIsComplete } from './governanceGuardrailPreset';

const URL = '/api/v2/admin/gateway/governance';
let created: Record<string, unknown>[] = [];

beforeEach(() => {
  created = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(http.post(URL, async ({ request }) => {
    created.push((await request.json()) as Record<string, unknown>);
    return HttpResponse.json({ id: String(created.length) }, { status: 201 });
  }));
});

afterEach(() => { resetGeneratedClient(); });

describe('presetEntries', () => {
  it('makes an input and an output entry, each with both packs', () => {
    const entries = presetEntries(DEFAULT_PRESET);
    expect(entries.map((e) => e.name)).toEqual(['Credentials and personal data - input', 'Credentials and personal data - output']);
    expect(entries[0]).toMatchObject({
      type: 'guardrail', enabled: true,
      data: { guardrail: { stage: 'input', detectors: [
        { name: 'credentials', type: 'secrets', action: 'block' },
        { name: 'personal-data', type: 'personal_data', action: 'mask' },
      ] } },
    });
    expect(entries[1]?.data).toMatchObject({ guardrail: { stage: 'output' } });
    // Platform-wide: no scope key at all.
    expect(entries[0]?.data).not.toHaveProperty('scope');
  });

  it('honours the pack, the action and the stage', () => {
    const entries = presetEntries({ ...DEFAULT_PRESET, name: ' PII ', credentials: false, personalDataAction: 'warn', output: false });
    expect(entries).toHaveLength(1);
    expect(entries[0]).toMatchObject({
      name: 'PII - input',
      data: { guardrail: { stage: 'input', detectors: [{ type: 'personal_data', action: 'warn' }] } },
    });
  });

  it('makes nothing without a name, a pack or a stage', () => {
    for (const bad of [{ name: '  ' }, { credentials: false, personalData: false }, { input: false, output: false }]) {
      const choice = { ...DEFAULT_PRESET, ...bad };
      expect(presetIsComplete(choice)).toBe(false);
      expect(presetEntries(choice)).toEqual([]);
    }
  });
});

describe('GovernanceGuardrailPresetDialog', () => {
  it('creates both entries through the create route and closes', async () => {
    const onClose = vi.fn();
    renderWithQueryAndTheme(<GovernanceGuardrailPresetDialog onClose={onClose} />);
    await userEvent.click(screen.getByTestId('governance-preset-create'));
    await waitFor(() => { expect(onClose).toHaveBeenCalled(); });
    expect(created).toHaveLength(2);
    expect(created[0]).toMatchObject({ type: 'guardrail', name: 'Credentials and personal data - input', enabled: true });
    expect(created[1]).toMatchObject({ name: 'Credentials and personal data - output' });
  });

  it('makes one entry when one stage is chosen', async () => {
    renderWithQueryAndTheme(<GovernanceGuardrailPresetDialog onClose={vi.fn()} />);
    await userEvent.click(screen.getByRole('checkbox', { name: 'Check the model’s answer' }));
    await userEvent.click(screen.getByTestId('governance-preset-create'));
    await waitFor(() => { expect(created).toHaveLength(1); });
    expect(created[0]).toMatchObject({ name: 'Credentials and personal data - input' });
  });

  it('disables Create until a pack and a stage are chosen', async () => {
    renderWithQueryAndTheme(<GovernanceGuardrailPresetDialog onClose={vi.fn()} />);
    await userEvent.click(screen.getByRole('checkbox', { name: 'Check what people send to the model' }));
    await userEvent.click(screen.getByRole('checkbox', { name: 'Check the model’s answer' }));
    expect(screen.getByTestId('governance-preset-create')).toBeDisabled();
  });

  it('keeps the screen open with the reason, and names what was made', async () => {
    const onClose = vi.fn();
    let calls = 0;
    server.use(http.post(URL, () => {
      calls += 1;
      return calls === 1
        ? HttpResponse.json({ id: '1' }, { status: 201 })
        : HttpResponse.json({ error: 'a governance entry with this section/type/name already exists' }, { status: 409 });
    }));
    renderWithQueryAndTheme(<GovernanceGuardrailPresetDialog onClose={onClose} />);
    await userEvent.click(screen.getByTestId('governance-preset-create'));
    expect(await screen.findByTestId('governance-preset-error')).toHaveTextContent(
      'Created Credentials and personal data - input, then failed: a governance entry with this section/type/name already exists');
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByTestId('governance-preset-create')).toBeEnabled();
  });
});
