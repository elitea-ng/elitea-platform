import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CredentialWarningBanner } from './CredentialWarningBanner';

describe('CredentialWarningBanner', () => {
  it('renders the credential-type sentence and a link to the given href', () => {
    renderWithTheme(
      <CredentialWarningBanner
        credentialId="github_shared_toolkit"
        credentialType="github"
        createHref="/credentials/create-credential/github?prefill_id=github_shared_toolkit"
      />,
    );
    expect(screen.getByText('Credential setup required:')).toBeInTheDocument();
    expect(screen.getByText(/This toolkit requires your own private github credentials\./)).toBeInTheDocument();
    const link = screen.getByRole('link', { name: 'Create a credential' });
    expect(link).toHaveAttribute('href', '/credentials/create-credential/github?prefill_id=github_shared_toolkit');
    expect(link).toHaveAttribute('target', '_blank');
    expect(screen.getByText(/with the matching ID "github_shared_toolkit"/)).toBeInTheDocument();
  });

  it('omits the credential-type sentence and the id clause when absent', () => {
    renderWithTheme(<CredentialWarningBanner createHref="/credentials/create-credential" />);
    expect(screen.queryByText(/This toolkit requires/)).not.toBeInTheDocument();
    expect(screen.getByText(/in your Private workspace to use this toolkit\./)).toBeInTheDocument();
  });

  it('fires onMount exactly once', () => {
    const onMount = vi.fn();
    const { rerender } = renderWithTheme(
      <CredentialWarningBanner
        createHref="/x"
        onMount={onMount}
      />,
    );
    rerender(
      <CredentialWarningBanner
        createHref="/x"
        onMount={onMount}
      />,
    );
    expect(onMount).toHaveBeenCalledTimes(1);
  });
});
