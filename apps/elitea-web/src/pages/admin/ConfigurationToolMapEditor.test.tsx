/**
 * The Guardrails map editor's pure parts.
 *
 * The value round trip and the duplicate detection are the two places this
 * control can lose an operator's work, so they are tested directly rather than
 * through the rendered form.
 */
import { useState } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import {
  canonicalConfigKey,
  ConfigurationToolMapEditor,
  duplicateToolkitRows,
  fromConfigToolMapRows,
  toConfigToolMapRows,
  type ConfigToolMapRow,
} from './ConfigurationToolMapEditor';
import { isToolMapField, widgetFor } from './configurationFields';

describe('canonicalConfigKey', () => {
  it('collapses case and separators the way the guardrail does', () => {
    for (const styled of ['Create File', 'create_file', 'create-file', 'CreateFile']) {
      expect(canonicalConfigKey(styled)).toBe('createfile');
    }
  });

  it('preserves the wildcard, which is not a toolkit name', () => {
    expect(canonicalConfigKey('*')).toBe('*');
    expect(canonicalConfigKey(' * ')).toBe('*');
  });

  it('reduces a separator-only value to nothing', () => {
    expect(canonicalConfigKey('---')).toBe('');
  });
});

describe('the value round trip', () => {
  it('reads a stored map into sorted rows', () => {
    expect(toConfigToolMapRows({ sharepoint: ['read'], github: ['create_issue'] })).toEqual([
      { toolkit: 'github', tools: ['create_issue'] },
      { toolkit: 'sharepoint', tools: ['read'] },
    ]);
  });

  it('survives a value the store should never have held', () => {
    // The server refuses these, so reaching them means someone wrote SQL. The
    // editor must still render something the operator can delete.
    expect(toConfigToolMapRows({ github: 'create_issue' })).toEqual([
      { toolkit: 'github', tools: [] },
    ]);
    expect(toConfigToolMapRows(['github'])).toEqual([]);
    expect(toConfigToolMapRows(null)).toEqual([]);
  });

  it('keeps a named toolkit with no tools, and drops an unnamed row', () => {
    // A named-but-empty row is a half-finished statement, not a mistake:
    // dropping it would delete the row on the keystroke that named it.
    expect(
      fromConfigToolMapRows([
        { toolkit: 'github', tools: [] },
        { toolkit: '   ', tools: ['create_issue'] },
      ]),
    ).toEqual({ github: [] });
  });

  it('trims and drops blank tool entries', () => {
    expect(fromConfigToolMapRows([{ toolkit: ' github ', tools: [' create_issue ', '  '] }])).toEqual(
      { github: ['create_issue'] },
    );
  });
});

describe('duplicateToolkitRows', () => {
  it('flags the LATER row, which is the one a JSON object would lose', () => {
    const rows = [
      { toolkit: 'GitHub', tools: ['a'] },
      { toolkit: 'git_hub', tools: ['b'] },
      { toolkit: 'sharepoint', tools: ['c'] },
    ];
    expect([...duplicateToolkitRows(rows)]).toEqual([1]);
  });

  it('does not flag blank rows against each other', () => {
    // Two rows the operator has just added are not a conflict.
    expect([...duplicateToolkitRows([{ toolkit: '', tools: [] }, { toolkit: '', tools: [] }])]).toEqual(
      [],
    );
  });
});

describe('widgetFor, for the guardrail maps', () => {
  const mapField = {
    key: 'blocked_tools',
    type: 'object',
    title: 'Blocked Tools',
    additionalProperties: { type: 'array', items: { type: 'string' } },
  } as const;

  it('renders a declared string-list map as the map editor', () => {
    expect(isToolMapField(mapField)).toBe(true);
    expect(widgetFor(mapField)).toBe('toolMap');
  });

  it('refuses an object whose value shape is undeclared', () => {
    // Its values could be anything, so an editor would invite the operator to
    // type what the consumer drops on the floor.
    const untyped = { key: 'mcp_servers', type: 'object', title: 'MCP Servers' } as const;
    expect(isToolMapField(untyped)).toBe(false);
    expect(widgetFor(untyped)).toBe('none');
  });

  it('still puts an unavailable reason ahead of the shape', () => {
    expect(widgetFor({ ...mapField, unavailable_reason: 'not here' })).toBe('unavailable');
  });
});

/**
 * A stateful harness: the real page owns `rows` in its own React Query cache
 * write, but this editor is a controlled component and the two issues below
 * are both about what happens to the DOM across a re-render, so the harness
 * has to actually hold state rather than pass a fixed `rows` prop.
 */
function Harness({ initialRows }: { readonly initialRows: readonly ConfigToolMapRow[] }) {
  const [rows, setRows] = useState<readonly ConfigToolMapRow[]>(initialRows);
  return (
    <ConfigurationToolMapEditor
      label="Sensitive Action Tools"
      rows={rows}
      disabled={false}
      toolkitOptions={['github', 'jira']}
      // `undefined` disables `useToolkitToolSuggestions`'s query outright (its
      // own `enabled` guard), so this harness needs no network/MSW mocking.
      toolSource={undefined}
      onChange={setRows}
    />
  );
}

function renderHarness(initialRows: readonly ConfigToolMapRow[] = [{ toolkit: '', tools: [] }]): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={queryClient}>
      <Harness initialRows={initialRows} />
    </QueryClientProvider>,
  );
}

describe('the rendered editor — issues 5986 and 5985 (Guardrails Sensitive Action Tools)', () => {
  /*
   * elitea_issues: #5986 — "Input field not cleared after dropdown selection"
   * describes a DIFFERENT mechanism than this editor has: a toolkit picker
   * that, on selecting a dropdown option, creates a separate "toolkit block"
   * above the input and leaves the input's own text behind, with a per-row
   * "Add" button that stays disabled. This editor has no such second step —
   * the toolkit `Autocomplete` is a single free-solo field whose
   * `onInputChange` writes straight into the row (`ConfigurationToolMapEditor.
   * tsx`'s `ToolMapRow`), so there is no confirmation step that could leave
   * stale text in an otherwise-committed input. NOT REPRODUCED on this UI.
   */
  it('typing (or selecting) a toolkit name updates the row directly — no leftover unconfirmed text, no per-row Add button', async () => {
    const user = userEvent.setup();
    renderHarness();

    const toolkitInput = screen.getByLabelText('Toolkit type');
    await user.type(toolkitInput, 'github');
    expect(toolkitInput).toHaveValue('github');

    // Only the group-level "Add toolkit — <label>" button exists (it adds a
    // NEW empty row); there is no separate per-row confirm/"Add" control that
    // the issue describes as staying disabled after a selection.
    expect(screen.getAllByRole('button', { name: /^Add toolkit/ })).toHaveLength(1);
    expect(screen.queryByRole('button', { name: /^Add$/ })).not.toBeInTheDocument();
  });

  /*
   * elitea_issues: #5985 — "No validation for non-existent toolkit names"
   * asks that the Add control be disabled (or an error shown) for a toolkit
   * name absent from the system's registry. `ConfigurationToolMapEditor.tsx`'s
   * own doc comment states this is deliberate: the registry supplies
   * SUGGESTIONS, never a whitelist, because `sensitive_tools` legitimately
   * accepts `*` and toolkit types the pinned SDK snapshot does not declare. A
   * closed picker would make an existing, working entry unrepresentable. NOT
   * REPRODUCED: this is documented design, not a missing guard.
   */
  it('an arbitrary (non-catalogued) toolkit name is accepted with no validation error and no disabled control', async () => {
    const user = userEvent.setup();
    renderHarness();

    const toolkitInput = screen.getByLabelText('Toolkit type');
    await user.type(toolkitInput, 'totally-nonexistent-toolkit');
    expect(toolkitInput).toHaveValue('totally-nonexistent-toolkit');
    expect(toolkitInput).not.toBeInvalid();

    const addButton = screen.getByRole('button', { name: 'Add toolkit — Sensitive Action Tools' });
    expect(addButton).toBeEnabled();

    // Adding a second row for the SAME (canonicalised) name is what this
    // editor actually validates against — a collision, not existence.
    await user.click(addButton);
    const rows = screen.getAllByLabelText('Toolkit type');
    expect(rows).toHaveLength(2);
    await user.type(rows[1] as HTMLElement, 'totally-nonexistent-toolkit');
    expect(within(screen.getByText(/Another row already covers this toolkit/)).getByText(/./)).toBeVisible();
  });
});
