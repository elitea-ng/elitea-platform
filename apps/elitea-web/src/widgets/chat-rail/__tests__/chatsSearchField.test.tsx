/**
 * The Chats search field takes the focus as it opens, and only then: a
 * re-render (a keystroke, a live frame) must not pull the focus back from
 * wherever the person moved it.
 */
import { useState } from 'react';

import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';

import type { ChatRailFilter } from '../model/chatRail';
import { ChatsSearchField } from '../ui/ChatsSearchField';

function Harness(): React.JSX.Element {
  const [query, setQuery] = useState('');
  const [searchOpen, setSearchOpen] = useState(true);
  const [ticks, setTicks] = useState(0);
  const filter: ChatRailFilter = { searchOpen, setSearchOpen, query, setQuery, localWorkOnly: false, setLocalWorkOnly: () => undefined };
  return (
    <>
      <button type="button" onClick={() => setTicks((n) => n + 1)}>
        rerender {ticks}
      </button>
      {searchOpen && <ChatsSearchField filter={filter} />}
    </>
  );
}

describe('ChatsSearchField', () => {
  it('focuses on opening and leaves the focus alone on a later render', async () => {
    const user = userEvent.setup();
    render(
      <AppProviders>
        <Harness />
      </AppProviders>,
    );
    const input = screen.getByTestId('shell-chats-search-input');
    expect(input).toHaveFocus();

    const other = screen.getByRole('button', { name: /rerender/ });
    await user.click(other);
    expect(other).toHaveFocus();
    expect(screen.getByRole('button', { name: 'rerender 1' })).toHaveFocus();
  });
});
