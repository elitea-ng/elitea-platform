import { StrictMode, useEffect, type ReactNode } from 'react';

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { useRegistrationStack, type RegistrationStack } from './registrationStack';

function Registers({ stack, value }: { readonly stack: RegistrationStack<string>; readonly value: string }): ReactNode {
  useEffect(() => stack.register(value), [stack, value]);
  return null;
}

function Host({ pages }: { readonly pages: readonly string[] }): ReactNode {
  const [stack, entries] = useRegistrationStack<string>();
  return (
    <>
      <div data-testid="entries">{entries.join(',')}</div>
      {pages.map((page) => (
        <Registers key={page} stack={stack} value={page} />
      ))}
    </>
  );
}

describe('registration stack', () => {
  it('keeps entries in registration order and withdraws exactly the one asked', () => {
    const view = render(<Host pages={['a', 'b']} />);
    expect(screen.getByTestId('entries')).toHaveTextContent('a,b');
    view.rerender(<Host pages={['b']} />);
    expect(screen.getByTestId('entries')).toHaveTextContent(/^b$/);
  });

  it('leaves no entry behind when StrictMode remounts and the page is swapped', () => {
    const view = render(
      <StrictMode>
        <Host pages={['archived']} />
      </StrictMode>,
    );
    expect(screen.getByTestId('entries')).toHaveTextContent(/^archived$/);
    view.rerender(
      <StrictMode>
        <Host pages={['agents']} />
      </StrictMode>,
    );
    expect(screen.getByTestId('entries')).toHaveTextContent(/^agents$/);
  });

  it('ignores a second withdrawal of the same entry', () => {
    let captured: RegistrationStack<string> | undefined;
    function Capture(): ReactNode {
      const [stack, entries] = useRegistrationStack<string>();
      captured = stack;
      return <div data-testid="entries">{entries.join(',')}</div>;
    }
    render(<Capture />);
    const stack = captured!;
    const withdrawA = stack.register('a');
    stack.register('b');
    withdrawA();
    withdrawA();
    expect(stack.snapshot()).toEqual(['b']);
  });
});
