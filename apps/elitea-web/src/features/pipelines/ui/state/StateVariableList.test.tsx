import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';
import { remToPx, renderWithTheme } from '@/shared/ui/lib/testTheme';

import { StateVariableList, type StateVariableListProps } from './StateVariableList';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function baseProps(overrides: Partial<StateVariableListProps> = {}): StateVariableListProps {
  return {
    states: undefined,
    onDeleteState: vi.fn(),
    onToggleState: vi.fn(),
    onUpdateState: vi.fn(),
    onAddState: vi.fn(() => true),
    ...overrides,
  };
}

describe('StateVariableList', () => {
  /* elitea_issues: #2657 — the "input" and "messages" default state rows must be visible in the Flow State view even before the yaml document declares them (`states` undefined). */
  it('always renders the input and messages rows', () => {
    renderWithTheme(<StateVariableList {...baseProps()} />);
    expect(screen.getByText('input')).toBeInTheDocument();
    expect(screen.getByText('messages')).toBeInTheDocument();
  });

  it('renders one row per custom state variable, excluding input/messages', () => {
    renderWithTheme(
      <StateVariableList
        {...baseProps({
          states: {
            input: { type: 'str' },
            messages: { type: 'list' },
            counter: { type: 'number', value: 0 },
          },
        })}
      />,
    );

    expect(screen.getByText('counter')).toBeInTheDocument();
  });

  it('opens a create row and calls onAddState with the typed name on blur', () => {
    const onAddState = vi.fn(() => true);
    renderWithTheme(<StateVariableList {...baseProps({ onAddState })} />);

    fireEvent.click(screen.getByRole('button', { name: 'Context' }));
    const input = screen.getByPlaceholderText('name');
    fireEvent.change(input, { target: { value: 'new_var' } });
    fireEvent.blur(input);

    expect(onAddState).toHaveBeenCalledWith('new_var', 'str');
  });

  it('renders the input_attachments row only when present in state', () => {
    const { rerender } = renderWithTheme(<StateVariableList {...baseProps()} />);
    expect(screen.queryByText('input_attachments')).not.toBeInTheDocument();

    rerender(
      <StateVariableList
        {...baseProps({ states: { input_attachments: { type: 'list' } } })}
      />,
    );
    expect(screen.getByText('input_attachments')).toBeInTheDocument();
  });

  // Baseline `addButton` style sets `fontSize: '.75rem'` (matched here off
  // `theme.typography.bodySmall.fontSize`, R-T11) — asserted directly on
  // the rendered button rather than by re-resolving `addButtonSx` in
  // isolation, since `fontSize` on a MUI `Button` only takes effect when
  // it actually lands on the rendered root.
  it('sizes the "Context" add button off the bodySmall typography fontSize', () => {
    renderWithTheme(<StateVariableList {...baseProps()} />);
    expect(screen.getByRole('button', { name: 'Context' })).toHaveStyle({
      // jsdom@30 resolves rem against the root font size before reporting a
      // computed length (jsdom@29 echoed the declaration back), so the computed
      // value is px while the theme still declares rem.
      fontSize: remToPx(theme.typography.bodySmall.fontSize),
    });
  });

  it('cancels (does not add) the create row when blurred with an empty name', () => {
    const onAddState = vi.fn();
    renderWithTheme(<StateVariableList {...baseProps({ onAddState })} />);

    fireEvent.click(screen.getByRole('button', { name: 'Context' }));
    const input = screen.getByPlaceholderText('name');
    fireEvent.blur(input);

    expect(onAddState).not.toHaveBeenCalled();
    // handleCancelCreate ran -> the create row is gone, "Context" is clickable again.
    expect(screen.queryByPlaceholderText('name')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Context' })).toBeInTheDocument();
  });

  it('renames a custom variable via handleUpdateNameWithCreate\'s edit (non-create) branch', () => {
    const onUpdateState = vi.fn();
    renderWithTheme(
      <StateVariableList
        {...baseProps({
          onUpdateState,
          states: { input: { type: 'str' }, messages: { type: 'list' }, counter: { type: 'number', value: 5 } },
        })}
      />,
    );

    fireEvent.click(screen.getByText('counter'));
    const input = screen.getByDisplayValue('counter');
    fireEvent.change(input, { target: { value: 'renamed' } });
    fireEvent.blur(input);

    expect(onUpdateState).toHaveBeenCalledWith('counter', { newName: 'renamed' });
  });

  it('handleDelete calls onDeleteState(name) for a custom variable', () => {
    const onDeleteState = vi.fn();
    renderWithTheme(
      <StateVariableList
        {...baseProps({ onDeleteState, states: { input: { type: 'str' }, messages: { type: 'list' }, counter: { type: 'number', value: 5 } } })}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    expect(onDeleteState).toHaveBeenCalledWith('counter');
  });

  describe('handleUpdateType', () => {
    it('preserves the current value when it is already set (not undefined/empty)', () => {
      const onUpdateState = vi.fn();
      renderWithTheme(
        <StateVariableList
          {...baseProps({
            onUpdateState,
            states: { input: { type: 'str' }, messages: { type: 'list' }, counter: { type: 'number', value: 5 } },
          })}
        />,
      );

      fireEvent.click(screen.getByRole('button', { name: 'Select data type' }));
      fireEvent.click(screen.getByRole('menuitem', { name: 'String' }));

      expect(onUpdateState).toHaveBeenCalledWith('counter', { type: 'str', value: 5 });
    });

    it('falls back to getDefaultValueForType(newType) when the current value is undefined', () => {
      const onUpdateState = vi.fn();
      renderWithTheme(
        <StateVariableList
          {...baseProps({
            onUpdateState,
            states: { input: { type: 'str' }, messages: { type: 'list' }, blank: { type: 'str' } },
          })}
        />,
      );

      fireEvent.click(screen.getByRole('button', { name: 'Select data type' }));
      fireEvent.click(screen.getByRole('menuitem', { name: 'Number' }));

      expect(onUpdateState).toHaveBeenCalledWith('blank', { type: 'number', value: 0 });
    });
  });

  describe('handleUpdateDefaultValue', () => {
    // Wide enough to render the default-value column as an inline `TextField`
    // (`StateVariableDefaultValue`'s `showAsField` gate, drawerWidth > 310),
    // not the narrow icon-button affordance.
    const WIDE_DRAWER = 400;

    it('a non-numeric-type (str) edit clears any validation error and passes the raw value straight through', () => {
      const onUpdateState = vi.fn();
      renderWithTheme(
        <StateVariableList
          {...baseProps({
            onUpdateState,
            drawerWidth: WIDE_DRAWER,
            states: { input: { type: 'str' }, messages: { type: 'list' }, note: { type: 'str', value: 'old' } },
          })}
        />,
      );

      const field = screen.getByDisplayValue('old');
      fireEvent.change(field, { target: { value: 'new text' } });

      expect(onUpdateState).toHaveBeenCalledWith('note', { value: 'new text' });
    });

    it('a valid numeric-type edit converts the value via getValueByType', () => {
      const onUpdateState = vi.fn();
      renderWithTheme(
        <StateVariableList
          {...baseProps({
            onUpdateState,
            drawerWidth: WIDE_DRAWER,
            states: { input: { type: 'str' }, messages: { type: 'list' }, counter: { type: 'number', value: 5 } },
          })}
        />,
      );

      const field = screen.getByDisplayValue('5');
      fireEvent.change(field, { target: { value: '42' } });

      expect(onUpdateState).toHaveBeenCalledWith('counter', { value: 42 });
    });

    it('an invalid numeric-type edit stores the raw value unconverted (early-return branch)', () => {
      const onUpdateState = vi.fn();
      renderWithTheme(
        <StateVariableList
          {...baseProps({
            onUpdateState,
            drawerWidth: WIDE_DRAWER,
            states: { input: { type: 'str' }, messages: { type: 'list' }, counter: { type: 'number', value: 5 } },
          })}
        />,
      );

      const field = screen.getByDisplayValue('5');
      fireEvent.change(field, { target: { value: 'not-a-number' } });

      expect(onUpdateState).toHaveBeenCalledWith('counter', { value: 'not-a-number' });
    });
  });
});
