/**
 * `useEliteaAssistantRef` / `SupportAssistantProvider` — the ref bridge that
 * lets a caller outside `../vendor/` open or close the assistant.
 */
import { useRef } from 'react';
import type { ReactNode } from 'react';

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { SupportAssistantProvider, useEliteaAssistantRef, type EliteaAssistantInstance } from './SupportAssistantContext';

function ReadRef({ label }: { readonly label: string }): ReactNode {
  const ref = useEliteaAssistantRef();
  return <span>{label}: {ref === null ? 'no provider' : 'has provider'}</span>;
}

describe('useEliteaAssistantRef', () => {
  it('returns null when called outside the provider', () => {
    render(<ReadRef label="outside" />);
    expect(screen.getByText('outside: no provider')).toBeInTheDocument();
  });

  it('returns the SAME ref object the provider was given', () => {
    function Wrapper(): ReactNode {
      const assistantRef = useRef<EliteaAssistantInstance | null>(null);
      return (
        <SupportAssistantProvider assistantRef={assistantRef}>
          <ReadRef label="inside" />
        </SupportAssistantProvider>
      );
    }
    render(<Wrapper />);
    expect(screen.getByText('inside: has provider')).toBeInTheDocument();
  });
});
