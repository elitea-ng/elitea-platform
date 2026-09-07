/*
 * The two credential-picker slots the toolkit form and the index schedule modal
 * are filled from.
 *
 * They had no test. Each is a small prop translation, and the two decisions in
 * them are the kind that shows up as "the credential list is empty" with
 * nothing in a log:
 *
 *  - `specifiedProjectId` WINS over the page's selected project. A toolkit
 *    shown in another project's context must list that project's credentials.
 *  - `schema.section` falls back to `credentials`, and `configuration_types`
 *    falls back to a SHARED empty array — a fresh `[]` per call would change
 *    the picker's prop identity on every render.
 *
 * The slot callbacks are called and their returned element's props read, rather
 * than rendered: mounting the real picker issues the credential list query,
 * which is that component's own test's subject, not this file's.
 */
import { renderHook } from '@testing-library/react';
import type { ReactElement } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { useScheduleCredentialsSelectSlot, useToolkitCredentialPickerSlot } from './credentialPickerSlots';

interface PickerProps {
  readonly projectId: string | undefined;
  readonly section: string;
  readonly configurationTypes: readonly string[];
  readonly value: unknown;
  readonly onChange: (value: unknown, options?: unknown) => void;
  readonly onlyPublic?: boolean;
  readonly field: {
    readonly label: string;
    readonly required?: boolean;
    readonly error: boolean;
    readonly helperText: string | undefined;
    readonly disabled: boolean;
  };
}

function toolkitSlot(projectId: string | undefined) {
  return renderHook(() => useToolkitCredentialPickerSlot(projectId)).result.current;
}

function fieldContext(overrides: Record<string, unknown> = {}) {
  return {
    schema: {},
    label: 'Credentials',
    required: true,
    disabled: false,
    error: false,
    helperText: undefined,
    value: { id: 3 },
    onChange: vi.fn(),
    specifiedProjectId: undefined,
    ...overrides,
  } as never;
}

describe('useToolkitCredentialPickerSlot', () => {
  it('uses the page project when the field names none', () => {
    const props = (toolkitSlot('7')(fieldContext()) as ReactElement).props as PickerProps;
    expect(props.projectId).toBe('7');
    expect(props.section).toBe('credentials');
    expect(props.configurationTypes).toEqual([]);
    expect(props.field).toEqual({
      label: 'Credentials',
      required: true,
      error: false,
      helperText: undefined,
      disabled: false,
    });
  });

  it('lets the field\'s own project win, as a string', () => {
    // The context carries a NUMBER here in the live app. A picker handed 42
    // instead of "42" reads no credentials and reports nothing.
    const props = (toolkitSlot('7')(fieldContext({ specifiedProjectId: 42 })) as ReactElement)
      .props as PickerProps;
    expect(props.projectId).toBe('42');
  });

  it('carries the schema\'s own section and configuration types', () => {
    const props = (
      toolkitSlot('7')(
        fieldContext({ schema: { section: 'auth', configuration_types: ['github'] } }),
      ) as ReactElement
    ).props as PickerProps;
    expect(props.section).toBe('auth');
    expect(props.configurationTypes).toEqual(['github']);
  });

  it('hands the SAME empty array to every type-less field', () => {
    const slot = toolkitSlot('7');
    const first = (slot(fieldContext()) as ReactElement).props as PickerProps;
    const second = (slot(fieldContext()) as ReactElement).props as PickerProps;
    // A fresh `[]` per call re-renders the picker on every parent render.
    expect(first.configurationTypes).toBe(second.configurationTypes);
  });

  it('keeps a stable slot identity while the project does not change', () => {
    const { result, rerender } = renderHook(({ id }: { id: string }) => useToolkitCredentialPickerSlot(id), {
      initialProps: { id: '7' },
    });
    const first = result.current;
    rerender({ id: '7' });
    expect(result.current).toBe(first);
    rerender({ id: '8' });
    expect(result.current).not.toBe(first);
  });

  it('forwards the field\'s own onChange, options and all', () => {
    const onChange = vi.fn();
    const props = (toolkitSlot('7')(fieldContext({ onChange })) as ReactElement).props as PickerProps;
    // The picker calls back with a second argument the toolkit form reads.
    props.onChange({ id: 9 }, { name: 'chosen' });
    expect(onChange).toHaveBeenCalledWith({ id: 9 }, { name: 'chosen' });
  });
});

describe('useScheduleCredentialsSelectSlot', () => {
  function scheduleSlot(projectId: string | undefined) {
    return renderHook(() => useScheduleCredentialsSelectSlot(projectId)).result.current;
  }

  it('always asks for the credentials section, and carries the modal\'s state', () => {
    const onChange = vi.fn();
    const props = (
      scheduleSlot('7')({
        value: { id: 4 },
        onChange,
        label: 'Credential',
        configurationTypes: ['confluence'],
        error: true,
        helperText: 'pick one',
        disabled: false,
        onlyPublic: true,
      }) as ReactElement
    ).props as PickerProps;

    // `IndexActions`' resolveCredentialsData finds the modal's property by
    // scanning for exactly this section name.
    expect(props.section).toBe('credentials');
    expect(props.projectId).toBe('7');
    expect(props.configurationTypes).toEqual(['confluence']);
    expect(props.onlyPublic).toBe(true);
    expect(props.field).toEqual({
      label: 'Credential',
      error: true,
      helperText: 'pick one',
      disabled: false,
    });
    // The modal's onChange takes ONE argument; the picker offers two. The
    // second is dropped here on purpose rather than forwarded into a callback
    // that does not declare it.
    props.onChange({ id: 5 }, { name: 'ignored' });
    expect(onChange).toHaveBeenCalledWith({ id: 5 });
  });

  it('keeps a stable slot identity while the project does not change', () => {
    const { result, rerender } = renderHook(({ id }: { id: string }) => useScheduleCredentialsSelectSlot(id), {
      initialProps: { id: '7' },
    });
    const first = result.current;
    rerender({ id: '7' });
    expect(result.current).toBe(first);
    rerender({ id: '8' });
    expect(result.current).not.toBe(first);
  });

  it('passes an absent project straight through rather than inventing one', () => {
    // A viewer with no project selected. The picker's own query is disabled on
    // an undefined project; a substituted default would list the wrong
    // project's credentials instead of listing none.
    const props = (
      scheduleSlot(undefined)({
        value: undefined,
        onChange: vi.fn(),
        label: 'Credential',
        configurationTypes: [],
        error: false,
        helperText: '',
        disabled: true,
        onlyPublic: false,
      }) as ReactElement
    ).props as PickerProps;
    expect(props.projectId).toBeUndefined();
    expect(props.onlyPublic).toBe(false);
    expect(props.field.disabled).toBe(true);
  });
});
