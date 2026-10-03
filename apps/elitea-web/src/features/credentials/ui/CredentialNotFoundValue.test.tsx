import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CredentialNotFoundValue } from './CredentialNotFoundValue';

describe('CredentialNotFoundValue', () => {
  it('renders the title without the tooltip icon before data has loaded', () => {
    renderWithTheme(
      <CredentialNotFoundValue
        eliteaTitle="missing-cred"
        hasFetchedData={false}
      />,
    );
    expect(screen.getByText('missing-cred')).toBeInTheDocument();
    expect(screen.queryByText('Credential not found')).not.toBeInTheDocument();
  });

  it('keeps the normal text colour and draws no attention icon once data has loaded (#6632)', () => {
    renderWithTheme(
      <CredentialNotFoundValue
        eliteaTitle="missing-cred"
        hasFetchedData
      />,
    );
    // The mismatch is the footer's message under the field. The field itself
    // shows the title in the normal colour, with no red and no icon.
    expect(screen.queryByLabelText('Credential not found')).not.toBeInTheDocument();
    const value = screen.getByTestId('credential-not-found-value');
    expect(value.querySelectorAll('svg')).toHaveLength(1);
    // The token, not the red `status.rejected` the value used to take.
    expect(getComputedStyle(screen.getByText('missing-cred')).color).toContain('text-secondary');
  });
});
