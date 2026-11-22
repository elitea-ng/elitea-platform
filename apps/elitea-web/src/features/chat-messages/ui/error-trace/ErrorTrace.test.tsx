import { describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ErrorTrace } from './ErrorTrace';

describe('ErrorTrace', () => {
  it('renders typed authorization, policy, and validation refusals with refusal status and reason', () => {
    const cases = [
      ['AUTHORIZATION_FAILED', 'Execution authorization failed.', 'Authorization refused', 'authorization'],
      ['CAP_REACHED', 'This run reached its tool-call limit.', 'Request refused by policy', 'policy'],
      ['UNSUPPORTED_CAPABILITY', 'Configuration type is not supported.', 'Request refused', 'validation'],
    ] as const;

    for (const [code, message, title, category] of cases) {
      const { unmount } = renderWithTheme(<ErrorTrace error={message} failureCode={code} />);
      const banner = screen.getByTestId('error-trace');
      expect(banner).toHaveAttribute('data-outcome', 'refusal');
      expect(banner).toHaveAttribute('data-refusal-category', category);
      expect(banner).toHaveTextContent(title);
      expect(banner).toHaveTextContent(message);
      unmount();
    }
  });

  it('replaces a raw refusal code headline with a calm fallback sentence', () => {
    renderWithTheme(<ErrorTrace error="AUTHORIZATION_FAILED" failureCode="AUTHORIZATION_FAILED" />);
    const banner = screen.getByTestId('error-trace');
    expect(banner).toHaveAttribute('data-outcome', 'refusal');
    expect(banner).toHaveTextContent('Authorization refused');
    expect(banner).toHaveTextContent('This run was refused because authorization failed.');
  });

  it('renders CANCELLED as a neutral stopped status', () => {
    renderWithTheme(<ErrorTrace error="Execution was cancelled." failureCode="CANCELLED" />);
    const banner = screen.getByTestId('error-trace');
    expect(banner).toHaveAttribute('data-outcome', 'cancelled');
    expect(banner).toHaveTextContent('Run stopped');
    expect(banner).toHaveTextContent('Execution was cancelled.');
  });

  it('keeps real runtime errors in the error banner', () => {
    renderWithTheme(<ErrorTrace error="The runtime operation failed." failureCode="INTERNAL" />);
    const banner = screen.getByTestId('error-trace');
    expect(banner).toHaveAttribute('data-outcome', 'error');
    expect(banner).toHaveTextContent('The runtime operation failed.');
  });

  it('keeps untyped errors in the error banner', () => {
    renderWithTheme(<ErrorTrace error="The agent run failed." />);
    const banner = screen.getByTestId('error-trace');
    expect(banner).toHaveAttribute('data-outcome', 'error');
    expect(banner).toHaveTextContent('The agent run failed.');
  });
});
