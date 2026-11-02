/**
 * The self-hosted classifier templates and the prompt-injection slot switch
 * of a guardrail (ADR-0043 decisions 1 and 4).
 */
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';

import { ThemeProvider } from '@mui/material/styles';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { GuardrailSelfHostedFields } from './GovernanceGuardrailSelfHostedFields';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderFields(detectors: unknown[] | string, isInput = true) {
  const onChange = vi.fn();
  render(
    <ThemeProvider theme={theme}>
      <GuardrailSelfHostedFields
        detectors={typeof detectors === 'string' ? detectors : JSON.stringify(detectors)}
        isInput={isInput}
        onDetectorsChange={onChange}
      />
    </ThemeProvider>,
  );
  return onChange;
}

function written(onChange: ReturnType<typeof vi.fn>): Record<string, unknown>[] {
  return JSON.parse(onChange.mock.calls.at(-1)?.[0] as string) as Record<string, unknown>[];
}

describe('GuardrailSelfHostedFields', () => {
  it('appends a Llama Guard template', () => {
    const onChange = renderFields([{ type: 'secrets', action: 'mask' }]);
    fireEvent.click(screen.getByTestId('governance-guardrail-add-template'));
    const list = written(onChange);
    expect(list).toHaveLength(2);
    expect(list[1]).toMatchObject({
      type: 'llama_guard',
      model: 'meta-llama/Llama-Guard-4-12B',
    });
  });

  it('puts a detector in the slot and turns mask into block', () => {
    const onChange = renderFields([
      {
        name: 'pi',
        type: 'http',
        action: 'mask',
        url: 'https://c.example.com',
      },
    ]);
    fireEvent.click(screen.getByTestId('governance-guardrail-slot-0'));
    expect(written(onChange)[0]).toMatchObject({
      action: 'block',
      content: 'tool_results',
      slot: 'prompt_injection',
    });
  });

  it('takes a detector out of the slot', () => {
    const onChange = renderFields([
      {
        name: 'pi',
        type: 'regex',
        action: 'flag',
        terms: ['x'],
        content: 'tool_results',
        slot: 'prompt_injection',
      },
    ]);
    fireEvent.click(screen.getByTestId('governance-guardrail-slot-0'));
    const [d] = written(onChange);
    expect(d?.slot).toBeUndefined();
    expect(d?.content).toBeUndefined();
  });

  it('does not offer the slot to a secrets detector', () => {
    renderFields([{ type: 'secrets', action: 'mask' }]);
    expect(screen.getByTestId('governance-guardrail-slot-0').querySelector('input')).toBeDisabled();
  });

  it('shows no slot switch on an output row', () => {
    renderFields([{ type: 'regex', action: 'flag', terms: ['x'] }], false);
    expect(screen.queryByTestId('governance-guardrail-slot-0')).toBeNull();
  });

  it('disables Add on invalid JSON', () => {
    renderFields('[{');
    expect(screen.getByTestId('governance-guardrail-add-template')).toBeDisabled();
  });
});
