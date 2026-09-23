/**
 * Two things about the accordion body, both easy to regress:
 *  - everything below the enable card is HIDDEN (not merely disabled) while
 *    context management is off — the baseline gates the whole block on
 *    `context_enabled`;
 *  - the copy really goes through `t()`. The lint gate cannot see it: the
 *    strings live inside JSX passed as the value of a JSX prop
 *    (`BasicAccordion items={[{ title, content }]}`), which
 *    `i18next/no-literal-string` does not descend into — a fallback renders
 *    identical text to a hardcoded literal, so a bundle override is the only
 *    proof (same reasoning as `profile/ProfileContextManagement.test.tsx`).
 */
import type { ReactElement } from 'react';

import { Formik } from 'formik';
import { screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { i18n } from '@/shared/i18n';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import {
  serializeSettingsProfile,
  type SettingsProfileFormValues,
} from '../ai-personality/settingsProfileForm';

import { MemoryContextManagement } from './MemoryContextManagement';

/** A throwaway locale, never an override on top of the shared `en` bundle. */
const TEST_LOCALE = 'zz-memory-ctx';

const OVERRIDES: Record<string, string> = {
  'settings.memory.contextManagement.title': 'TITLE-XX',
  'settings.memory.contextManagement.cardDescription': 'ENABLE-XX',
  'contextBudget.mode.title': 'MAXTOKENS-XX',
  'settings.memory.contextManagement.preserveRecentMessages': 'PRESERVE-XX',
  'settings.memory.contextEditing.description': 'EDITING-XX',
  'settings.memory.contextManagement.enableAriaLabel': 'ARIA-XX',
};

function renderSection(overrides: Partial<SettingsProfileFormValues> = {}): void {
  const values: SettingsProfileFormValues = { ...serializeSettingsProfile(undefined), ...overrides };
  renderWithTheme(
    (
      <Formik<SettingsProfileFormValues>
        initialValues={values}
        onSubmit={() => undefined}
      >
        <MemoryContextManagement />
      </Formik>
    ) as ReactElement,
  );
}

afterEach(async () => {
  await i18n.changeLanguage('en');
  i18n.removeResourceBundle(TEST_LOCALE, 'translation');
});

describe('MemoryContextManagement', () => {
  it('hides every control below the enable card while context management is off', () => {
    renderSection({ context_enabled: false });

    expect(screen.getByLabelText('Enable context management')).toBeInTheDocument();
    expect(screen.queryByTestId('context-budget-mode-control')).not.toBeInTheDocument();
    expect(screen.queryByText('Context Editing')).not.toBeInTheDocument();
    expect(screen.queryByText('Automatic Summarization')).not.toBeInTheDocument();
  });

  it('reveals the fields, the context-editing card and the summarization block when on', () => {
    renderSection({ context_enabled: true });

    expect(screen.getByTestId('context-budget-mode-control')).toBeInTheDocument();
    expect(screen.getByTestId('preserve-recent-messages-input')).toBeInTheDocument();
    expect(screen.getByText('Context Editing')).toBeInTheDocument();
    expect(screen.getByText('Automatic Summarization')).toBeInTheDocument();
  });

  it('renders every visible string from the bundle, not from a literal', async () => {
    i18n.addResourceBundle(TEST_LOCALE, 'translation', OVERRIDES, true, true);
    await i18n.changeLanguage(TEST_LOCALE);

    renderSection({ context_enabled: true });

    expect(screen.getByText('TITLE-XX')).toBeInTheDocument();
    expect(screen.getByText('ENABLE-XX')).toBeInTheDocument();
    expect(screen.getAllByText('MAXTOKENS-XX')[0]).toBeInTheDocument();
    expect(screen.getByText('PRESERVE-XX')).toBeInTheDocument();
    expect(screen.getByText('EDITING-XX')).toBeInTheDocument();
    expect(screen.getByLabelText('ARIA-XX')).toBeInTheDocument();
  });
});
