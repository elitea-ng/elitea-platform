import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { YamlPipelineDocument } from '../../lib/flow-editor/helpers/pipelineFlow.types';

import { StateDrawer } from './StateDrawer';

describe('StateDrawer', () => {
  it('renders nothing when closed', () => {
    renderWithTheme(
      <StateDrawer
        isOpen={false}
        onClose={vi.fn()}
        setYamlJsonObject={vi.fn()}
        yamlJsonObject={{}}
      />,
    );
    expect(screen.queryByText('STATE')).not.toBeInTheDocument();
  });

  it('renders the header and default rows when open', () => {
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={vi.fn()}
        yamlJsonObject={{}}
      />,
    );
    expect(screen.getByText('STATE')).toBeInTheDocument();
    expect(screen.getByText('input')).toBeInTheDocument();
    expect(screen.getByText('messages')).toBeInTheDocument();
  });

  it('calls onClose when the close button is clicked', () => {
    const onClose = vi.fn();
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={onClose}
        setYamlJsonObject={vi.fn()}
        yamlJsonObject={{}}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('adds a new state variable, merging it into the existing document state', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    const yamlJsonObject: YamlPipelineDocument = { state: { input: { type: 'str' }, messages: { type: 'list' } } };
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={yamlJsonObject}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Context' }));
    const input = screen.getByPlaceholderText('name');
    fireEvent.change(input, { target: { value: 'counter' } });
    fireEvent.blur(input);

    const updatedDocument = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(updatedDocument.state?.['counter']).toMatchObject({ type: 'str' });
  });

  /* elitea_issues: #2662 — toggling the "input"/"messages" default state rows must persist to yamlJsonObject.state (survive Save/reload), not silently revert. */
  it('toggles the messages row off, removing it from state', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    const yamlJsonObject: YamlPipelineDocument = { state: { input: { type: 'str' }, messages: { type: 'list' } } };
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={yamlJsonObject}
      />,
    );

    const toggles = screen.getAllByRole('switch');
    fireEvent.click(toggles[1] as HTMLElement);

    const updatedDocument = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(Object.keys(updatedDocument.state ?? {})).not.toContain('messages');
  });

  it('toggles the messages row back on as a List type (re-enable branch, name === STATE_MESSAGES)', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    const yamlJsonObject: YamlPipelineDocument = { state: { input: { type: 'str' } } };
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={yamlJsonObject}
      />,
    );

    const toggles = screen.getAllByRole('switch');
    fireEvent.click(toggles[1] as HTMLElement);

    const updatedDocument = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(updatedDocument.state?.['messages']).toMatchObject({ type: 'list' });
    // The pre-existing "input" entry survives untouched.
    expect(updatedDocument.state?.['input']).toMatchObject({ type: 'str' });
  });

  it('re-enables the (currently off) input row as a String type, preserving the existing "messages" entry', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    // `input` is deliberately absent from `state` so `StateVariableList` computes its
    // toggle as OFF (`!states || Boolean(states[STATE_INPUT])` -> false here) — clicking
    // it exercises the "enable" (not "disable") branch of `handleToggleState`.
    const yamlJsonObject: YamlPipelineDocument = { state: { messages: { type: 'list' } } };
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={yamlJsonObject}
      />,
    );

    const toggles = screen.getAllByRole('switch');
    fireEvent.click(toggles[0] as HTMLElement);

    const updatedDocument = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(updatedDocument.state?.['input']).toMatchObject({ type: 'str' });
    expect(updatedDocument.state?.['messages']).toMatchObject({ type: 'list' });
  });

  it('renames a custom state variable via handleUpdateState (newName branch), preserving its config under the new key', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    const yamlJsonObject: YamlPipelineDocument = {
      state: { input: { type: 'str' }, messages: { type: 'list' }, counter: { type: 'number', value: 3 } },
    };
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={yamlJsonObject}
      />,
    );

    fireEvent.click(screen.getByText('counter'));
    const input = screen.getByDisplayValue('counter');
    fireEvent.change(input, { target: { value: 'renamed_counter' } });
    fireEvent.blur(input);

    const updatedDocument = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(updatedDocument.state?.['renamed_counter']).toMatchObject({ type: 'number', value: 3 });
    expect(updatedDocument.state?.['counter']).toBeUndefined();
  });

  it('deletes a custom state variable via handleDeleteState', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    const yamlJsonObject: YamlPipelineDocument = { state: { input: { type: 'str' }, counter: { type: 'number', value: 3 } } };
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={yamlJsonObject}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));

    const updatedDocument = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(Object.keys(updatedDocument.state ?? {})).not.toContain('counter');
    expect(updatedDocument.state?.['input']).toMatchObject({ type: 'str' });
  });

  // `handleAddState`'s own internal guards for a duplicate name and for a
  // name that fails the identifier regex (`StateDrawer.tsx`'s `if
  // (currentState?.[name]) return false;` / `if (!/^[a-zA-Z][a-zA-Z0-9_]*
  // $/.test(name)) return false;`) are defensive-only through this real,
  // composed UI: `StateVariableList` always wires the SAME `validateVariableName`
  // check (identical duplicate + regex rules) as the create-mode item's own
  // `validateName` prop, and `StateVariableItem.controller.ts`'s
  // `handleNameBlur` only calls `onUpdateName` (which reaches `onAddState`)
  // when that upstream check already found no error — so a name that would
  // trip either of `handleAddState`'s own guards is always intercepted one
  // layer up first (the create row is cancelled via `onCancel`, `onAddState`
  // is never invoked). Confirmed directly: typing either an already-used or
  // an invalid-pattern name below never calls `setYamlJsonObject`, but it is
  // the upstream `validateName` guard doing the work, not `handleAddState`'s
  // own — those two lines are unreachable dead-guard code through the
  // composed UI, not exercisable by a behavioural test at this level.
  it('typing an already-used name into the create row is rejected upstream (never reaches setYamlJsonObject)', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    const yamlJsonObject: YamlPipelineDocument = { state: { input: { type: 'str' }, counter: { type: 'number', value: 3 } } };
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={yamlJsonObject}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Context' }));
    const input = screen.getByPlaceholderText('name');
    fireEvent.change(input, { target: { value: 'counter' } });
    fireEvent.blur(input);

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it('typing a name that fails the identifier pattern into the create row is rejected upstream (never reaches setYamlJsonObject)', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={{}}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Context' }));
    const input = screen.getByPlaceholderText('name');
    fireEvent.change(input, { target: { value: '1bad' } });
    fireEvent.blur(input);

    expect(setYamlJsonObject).not.toHaveBeenCalled();
  });

  it('handleAddState seeds a brand-new document from DefaultState when yamlJsonObject.state is entirely absent', () => {
    const setYamlJsonObject = vi.fn<(document: YamlPipelineDocument) => void>();
    renderWithTheme(
      <StateDrawer
        isOpen
        onClose={vi.fn()}
        setYamlJsonObject={setYamlJsonObject}
        yamlJsonObject={{}}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Context' }));
    const input = screen.getByPlaceholderText('name');
    fireEvent.change(input, { target: { value: 'fresh_var' } });
    fireEvent.blur(input);

    const updatedDocument = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(updatedDocument.state?.['fresh_var']).toMatchObject({ type: 'str' });
    expect(updatedDocument.state?.['input']).toMatchObject({ type: 'str' });
    expect(updatedDocument.state?.['messages']).toMatchObject({ type: 'list' });
  });
});
