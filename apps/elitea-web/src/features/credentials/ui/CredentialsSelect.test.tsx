import { fireEvent, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CredentialsSelect } from './CredentialsSelect';
import type { CredentialOptionRow } from './CredentialsSelect';

const ROWS: CredentialOptionRow[] = [
  { eliteaTitle: 'my-openai', isPrivate: true, displayLabel: 'My OpenAI' },
  { eliteaTitle: 'shared-azure', isPrivate: false, displayLabel: 'Shared Azure', shared: true },
];

function baseState(overrides: Partial<Parameters<typeof CredentialsSelect>[0]['state']> = {}) {
  return {
    configurations: ROWS,
    hasFetchedData: true,
    isFetching: false,
    getStatus: () => 'idle' as const,
    getMessage: () => '',
    ...overrides,
  };
}

function baseHandlers(overrides: Partial<Parameters<typeof CredentialsSelect>[0]['handlers']> = {}) {
  return {
    onSelect: vi.fn(),
    onRefresh: vi.fn(),
    onCreate: vi.fn(),
    onRevalidate: vi.fn(),
    ...overrides,
  };
}

describe('CredentialsSelect', () => {
  it('renders the label and shows the currently selected row', () => {
    renderWithTheme(
      <CredentialsSelect
        value={{ eliteaTitle: 'my-openai', isPrivate: true }}
        state={baseState()}
        handlers={baseHandlers()}
        field={{ label: 'API Credential' }}
      />,
    );
    expect(screen.getByText('API Credential')).toBeInTheDocument();
    expect(screen.getByText('My OpenAI')).toBeInTheDocument();
  });

  it('lists both create actions and every saved row when the menu opens', () => {
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState()}
        handlers={baseHandlers()}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('combobox'));
    const listbox = within(screen.getByRole('listbox'));
    expect(listbox.getByText('New private credentials')).toBeInTheDocument();
    expect(listbox.getByText('New project credentials')).toBeInTheDocument();
    expect(listbox.getByText('My OpenAI')).toBeInTheDocument();
    expect(listbox.getByText('Shared Azure')).toBeInTheDocument();
  });

  it('offers a search/filter field in the popup, that narrows the saved rows (issue 919/ELITEA-2534)', () => {
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState()}
        handlers={baseHandlers()}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('combobox'));
    const listbox = within(screen.getByRole('listbox'));
    const search = listbox.getByPlaceholderText('Search credentials');
    expect(search).toBeInTheDocument();

    fireEvent.change(search, { target: { value: 'azure' } });
    expect(listbox.queryByText('My OpenAI')).not.toBeInTheDocument();
    expect(listbox.getByText('Shared Azure')).toBeInTheDocument();
    // The CREATE rows are not rows being searched — they stay offered.
    expect(listbox.getByText('New private credentials')).toBeInTheDocument();
  });

  it('matching is case-insensitive and clearing the query restores every row', () => {
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState()}
        handlers={baseHandlers()}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('combobox'));
    const listbox = within(screen.getByRole('listbox'));
    const search = listbox.getByPlaceholderText('Search credentials');

    fireEvent.change(search, { target: { value: 'OPENAI' } });
    expect(listbox.getByText('My OpenAI')).toBeInTheDocument();
    expect(listbox.queryByText('Shared Azure')).not.toBeInTheDocument();

    fireEvent.change(search, { target: { value: '' } });
    expect(listbox.getByText('My OpenAI')).toBeInTheDocument();
    expect(listbox.getByText('Shared Azure')).toBeInTheDocument();
  });

  it('omits the search field entirely when there are no saved rows to filter', () => {
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState({ configurations: [] })}
        handlers={baseHandlers()}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('combobox'));
    expect(screen.queryByPlaceholderText('Search credentials')).not.toBeInTheDocument();
  });

  it('selecting a saved row calls onSelect with its identity', () => {
    const onSelect = vi.fn();
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState()}
        handlers={baseHandlers({ onSelect })}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('combobox'));
    fireEvent.click(within(screen.getByRole('listbox')).getByText('Shared Azure'));
    expect(onSelect).toHaveBeenCalledWith({ eliteaTitle: 'shared-azure', isPrivate: false }, { isAutoSelect: false });
  });

  it('re-selecting the already-selected row clears the value', () => {
    const onSelect = vi.fn();
    renderWithTheme(
      <CredentialsSelect
        value={{ eliteaTitle: 'my-openai', isPrivate: true }}
        state={baseState()}
        handlers={baseHandlers({ onSelect })}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('combobox'));
    fireEvent.click(within(screen.getByRole('listbox')).getByText('My OpenAI'));
    expect(onSelect).toHaveBeenCalledWith(null, { isAutoSelect: false });
  });

  it('selecting a create action calls onCreate with the right privacy flag', () => {
    const onCreate = vi.fn();
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState()}
        handlers={baseHandlers({ onCreate })}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('combobox'));
    fireEvent.click(within(screen.getByRole('listbox')).getByText('New project credentials'));
    expect(onCreate).toHaveBeenCalledWith(false);
  });

  it('omits create actions when isCreationAllowed is false', () => {
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState()}
        handlers={baseHandlers()}
        isCreationAllowed={false}
      />,
    );
    fireEvent.mouseDown(screen.getByRole('combobox'));
    expect(screen.queryByText('New private credentials')).not.toBeInTheDocument();
  });

  it('clicking refresh calls onRefresh, and disables the button while fetching', () => {
    const onRefresh = vi.fn();
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState({ isFetching: true })}
        handlers={baseHandlers({ onRefresh })}
      />,
    );
    const button = screen.getByTestId('credentials-select-refresh');
    expect(button).toBeDisabled();
  });

  it('renders CredentialNotFoundValue when the value matches no loaded row', () => {
    renderWithTheme(
      <CredentialsSelect
        value={{ eliteaTitle: 'missing-cred', isPrivate: true }}
        state={baseState()}
        handlers={baseHandlers()}
      />,
    );
    expect(screen.getByText('missing-cred')).toBeInTheDocument();
  });

  it('shows the mismatch footer when the value is unmatched and mismatch info is supplied', () => {
    renderWithTheme(
      <CredentialsSelect
        value={{ eliteaTitle: 'missing-cred', isPrivate: true }}
        state={baseState()}
        handlers={baseHandlers()}
        mismatch={{ mismatchedPrivateCredential: false, createHref: '/x' }}
      />,
    );
    expect(screen.getByText('Your configuration does not match any available configurations.')).toBeInTheDocument();
  });

  it('auto-selects the first row once when autoSelectFirstShared is set and the value is blank', () => {
    const onSelect = vi.fn();
    renderWithTheme(
      <CredentialsSelect
        value={null}
        state={baseState()}
        handlers={baseHandlers({ onSelect })}
        autoSelectFirstShared
      />,
    );
    expect(onSelect).toHaveBeenCalledWith({ eliteaTitle: 'my-openai', isPrivate: true }, { isAutoSelect: true });
  });

  it('does not auto-select when a value is already present', () => {
    const onSelect = vi.fn();
    renderWithTheme(
      <CredentialsSelect
        value={{ eliteaTitle: 'shared-azure', isPrivate: false }}
        state={baseState()}
        handlers={baseHandlers({ onSelect })}
        autoSelectFirstShared
      />,
    );
    expect(onSelect).not.toHaveBeenCalled();
  });
});
