/**
 * DEFECT: the Context Management section bypassed the i18n layer entirely.
 *
 * `ProfileContextManagement.tsx` had no `@/shared/i18n` import at all. The
 * accordion title, the switch label and aria-label, and both field
 * label/tooltip pairs were raw English, on a live page (`/settings/
 * personalization`) whose surrounding copy already goes through `t()`. No
 * locale bundle could override them, and none of the copy reached `en.json`,
 * so it could not be centrally edited either.
 *
 * The lint gate did not catch it and still cannot: `i18next/no-literal-string`
 * does not descend into JSX that is the value of a JSX prop, which is exactly
 * the `BasicAccordion items={[{ title, content: <…/> }]}` shape this file
 * uses. A bundle override is therefore the only reliable proof that the
 * strings really go through `t()` — a fallback renders identical text to a
 * hardcoded literal.
 */
import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Formik } from 'formik';
import { screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { PROFILE_INITIAL_VALUES } from '@/features/settings/lib/profile/profileUtils';
import { i18n } from '@/shared/i18n';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ProfileContextManagement } from './ProfileContextManagement';

/**
 * A throwaway locale, NOT an override on top of `en`. `addResourceBundle`
 * with `deep: true` merges into the live store object, which is the very
 * `en.json` module object every other suite shares — `removeResourceBundle`
 * does not undo that merge, so seeding `en` here would leak into the rest of
 * the run.
 */
const TEST_LOCALE = 'zz-profile-ctx';

/** Copy every visible string in the section to a value no literal could produce. */
const OVERRIDES: Record<string, string> = {
  'settings.profile.contextManagement.title': 'TITLE-XX',
  'settings.profile.contextManagement.enableAriaLabel': 'ARIA-XX',
  'settings.profile.contextManagement.enableLabel': 'ENABLE-XX',
  'settings.profile.contextManagement.maxContextTokens': 'MAXTOKENS-XX',
  'settings.profile.contextManagement.preserveRecentMessages': 'PRESERVE-XX',
};

// ProfileContextManagement renders `ProfileLongTermMemory` (#870), which is
// a real, data-fetching component now (Settings > Memory's "Long-term
// Memory" panel — see that component's own header for why this dead route,
// `/settings/personalization`, still gets the real feature rather than the
// old "Coming soon" placeholder). A QueryClient is therefore required to
// mount this section at all, even though this suite's own assertions are
// about UNRELATED context-management copy.
function renderSection(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    (
      <QueryClientProvider client={queryClient}>
        <Formik
          initialValues={PROFILE_INITIAL_VALUES}
          onSubmit={() => undefined}
        >
          <ProfileContextManagement modelList={[]} />
        </Formik>
      </QueryClientProvider>
    ) as ReactElement,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(async () => {
  resetGeneratedClient();
  await i18n.changeLanguage('en');
  i18n.removeResourceBundle(TEST_LOCALE, 'translation');
});

describe('ProfileContextManagement — i18n', () => {
  it('renders every visible string from the bundle, not from a literal', async () => {
    i18n.addResourceBundle(TEST_LOCALE, 'translation', OVERRIDES, true, true);
    await i18n.changeLanguage(TEST_LOCALE);

    renderSection();

    expect(screen.getByText('TITLE-XX')).toBeInTheDocument();
    expect(screen.getByText('ENABLE-XX')).toBeInTheDocument();
    expect(screen.getByText('MAXTOKENS-XX')).toBeInTheDocument();
    expect(screen.getByText('PRESERVE-XX')).toBeInTheDocument();
    expect(screen.getByLabelText('ARIA-XX')).toBeInTheDocument();
  });

  it('falls back to the English copy when the bundle has no key', () => {
    renderSection();

    expect(screen.getByText('Default Context Management')).toBeInTheDocument();
    expect(screen.getByText('Max Context Tokens')).toBeInTheDocument();
  });
});
