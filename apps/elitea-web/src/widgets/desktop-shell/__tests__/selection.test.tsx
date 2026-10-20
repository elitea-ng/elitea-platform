import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';

import { DesktopThemeLayer } from '../ui/DesktopThemeLayer';

describe('what the desktop selects', () => {
  it('leaves content selectable and keeps only the chrome from selecting', () => {
    render(
      <AppProviders>
        <DesktopThemeLayer>
          <p>An error you may want to copy</p>
          <button type="button">Send</button>
          <nav aria-label="folders">Local work</nav>
        </DesktopThemeLayer>
      </AppProviders>,
    );
    const select = (el: Element): string => getComputedStyle(el).userSelect || getComputedStyle(el).getPropertyValue('-webkit-user-select');
    expect(select(screen.getByText('An error you may want to copy'))).not.toBe('none');
    expect(select(document.body)).not.toBe('none');
    expect(select(screen.getByRole('button', { name: 'Send' }))).toBe('none');
    expect(select(screen.getByRole('navigation'))).toBe('none');
  });
});
