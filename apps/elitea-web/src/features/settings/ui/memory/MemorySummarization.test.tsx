/**
 * The summarization disable-gating. Two DIFFERENT conditions are at work in
 * the same block, which is exactly the kind of thing a rewrite gets wrong:
 *
 *  - the "Automatic Summarization" switch follows `context_enabled` alone
 *    (it must stay operable so the user can turn summarization back on);
 *  - the instructions field and the target-tokens field are disabled when
 *    EITHER `context_enabled` or `enable_summarization` is off.
 *
 * Gating them all on one flag — the obvious simplification — passes a render
 * test and breaks the page in one of the four states below.
 */
import type { ReactElement } from 'react';

import { Formik } from 'formik';
import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import {
  serializeSettingsProfile,
  type SettingsProfileFormValues,
} from '../ai-personality/settingsProfileForm';

import { MemorySummarization } from './MemorySummarization';

const INSTRUCTIONS_LABEL = 'Additional summary guidance (optional)';
const TARGET_TOKENS_LABEL = 'Target Summary Tokens';
const SWITCH_LABEL = 'Enable automatic summarization';

function renderBlock(overrides: Partial<SettingsProfileFormValues>): void {
  const values: SettingsProfileFormValues = { ...serializeSettingsProfile(undefined), ...overrides };
  renderWithTheme(
    (
      <Formik<SettingsProfileFormValues>
        initialValues={values}
        onSubmit={() => undefined}
      >
        <MemorySummarization />
      </Formik>
    ) as ReactElement,
  );
}

describe('MemorySummarization — disable gating', () => {
  it('enables both fields only when context management AND summarization are on', () => {
    renderBlock({ context_enabled: true, enable_summarization: true });

    expect(screen.getByLabelText(INSTRUCTIONS_LABEL)).toBeEnabled();
    expect(screen.getByLabelText(TARGET_TOKENS_LABEL)).toBeEnabled();
    expect(screen.getByLabelText(SWITCH_LABEL)).toBeEnabled();
  });

  it('disables both fields when summarization is off but context management is on', () => {
    renderBlock({ context_enabled: true, enable_summarization: false });

    expect(screen.getByLabelText(INSTRUCTIONS_LABEL)).toBeDisabled();
    expect(screen.getByLabelText(TARGET_TOKENS_LABEL)).toBeDisabled();
    // The switch itself must stay operable — it is how the fields come back.
    expect(screen.getByLabelText(SWITCH_LABEL)).toBeEnabled();
  });

  it('disables both fields AND the switch when context management is off', () => {
    renderBlock({ context_enabled: false, enable_summarization: true });

    expect(screen.getByLabelText(INSTRUCTIONS_LABEL)).toBeDisabled();
    expect(screen.getByLabelText(TARGET_TOKENS_LABEL)).toBeDisabled();
    expect(screen.getByLabelText(SWITCH_LABEL)).toBeDisabled();
  });

  it('disables everything when both flags are off', () => {
    renderBlock({ context_enabled: false, enable_summarization: false });

    expect(screen.getByLabelText(INSTRUCTIONS_LABEL)).toBeDisabled();
    expect(screen.getByLabelText(TARGET_TOKENS_LABEL)).toBeDisabled();
    expect(screen.getByLabelText(SWITCH_LABEL)).toBeDisabled();
  });
});
