import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CredentialOptionLabel } from './CredentialOptionLabel';

describe('CredentialOptionLabel', () => {
  it('renders the label and the personal-owner icon', () => {
    renderWithTheme(<CredentialOptionLabel isPersonal label="My credential" />);
    expect(screen.getByText('My credential')).toBeInTheDocument();
  });

  it('shows the attention indicator with the invalid message as its accessible name', () => {
    renderWithTheme(
      <CredentialOptionLabel
        isPersonal={false}
        label="Team credential"
        isInvalid
        invalidMessage="bad key"
      />,
    );
    expect(screen.getByTestId('credential-status-indicator')).toHaveAttribute('aria-label', 'bad key');
  });

  it('falls back to the default message when isInvalid has no message', () => {
    renderWithTheme(
      <CredentialOptionLabel
        isPersonal={false}
        label="Team credential"
        isInvalid
      />,
    );
    expect(screen.getByTestId('credential-status-indicator')).toHaveAttribute(
      'aria-label',
      'Credential is unavailable or misconfigured',
    );
  });

  it('renders the open-in-new-tab button only when credentialUrl is set, and opens it without navigating the option row', () => {
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null);
    renderWithTheme(
      <CredentialOptionLabel
        isPersonal
        label="x"
        credentialUrl="https://example.com/cred"
      />,
    );
    const button = screen.getByTestId('credential-open-in-new-tab-button');
    fireEvent.click(button);
    expect(openSpy).toHaveBeenCalledWith('https://example.com/cred', '_blank', 'noopener,noreferrer');
    openSpy.mockRestore();
  });

  it('omits the open-in-new-tab button when credentialUrl is absent', () => {
    renderWithTheme(
      <CredentialOptionLabel
        isPersonal
        label="x"
      />,
    );
    expect(screen.queryByTestId('credential-open-in-new-tab-button')).not.toBeInTheDocument();
  });

  it('calls onRevalidate from the reload button, and disables it while checking', () => {
    const onRevalidate = vi.fn();
    renderWithTheme(
      <CredentialOptionLabel
        isPersonal
        label="x"
        isInvalid
        isChecking
        onRevalidate={onRevalidate}
      />,
    );
    const reload = screen.getByTestId('credential-reload-button');
    expect(reload).toBeDisabled();
  });

  it('puts the row actions before the attention icon, which comes last (#6632)', () => {
    renderWithTheme(
      <CredentialOptionLabel
        isPersonal={false}
        label="Team credential"
        credentialUrl="https://example.test/credentials/1"
        isInvalid
        onRevalidate={() => {}}
      />,
    );
    const indicator = screen.getByTestId('credential-status-indicator');
    const actions = screen.getByTestId('credential-option-actions');
    expect(actions.contains(screen.getByTestId('credential-open-in-new-tab-button'))).toBe(true);
    expect(actions.contains(screen.getByTestId('credential-reload-button'))).toBe(true);
    // DOCUMENT_POSITION_FOLLOWING: the indicator comes after the actions.
    expect(actions.compareDocumentPosition(indicator) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(indicator.parentElement?.lastElementChild).toBe(indicator);
  });
});
