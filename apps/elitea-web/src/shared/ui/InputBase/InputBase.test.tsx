import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { InputBase } from '.';

describe('InputBase', () => {
  it('renders a plain string label via the native TextField label', () => {
    const { getByLabelText } = renderWithTheme(
      <InputBase
        label="Name"
        value=""
        onChange={() => {}}
      />,
    );
    expect(getByLabelText('Name')).toBeInTheDocument();
  });

  it('propagates onChange from the underlying input', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByLabelText } = renderWithTheme(
      <InputBase
        label="Name"
        value=""
        onChange={onChange}
      />,
    );
    await user.type(getByLabelText('Name'), 'a');
    expect(onChange).toHaveBeenCalled();
  });

  it('renders a single-line input when no expand config is given', () => {
    const { getByLabelText } = renderWithTheme(
      <InputBase
        label="Name"
        value=""
        onChange={() => {}}
      />,
    );
    expect(getByLabelText('Name').tagName).toBe('INPUT');
  });

  it('renders a multiline textarea when expand config is given', () => {
    const { getByLabelText } = renderWithTheme(
      <InputBase
        label="Description"
        value=""
        onChange={() => {}}
        expand={{ minRows: 2, maxRows: 6 }}
      />,
    );
    expect(getByLabelText('Description').tagName).toBe('TEXTAREA');
  });

  it('renders the info-icon tooltip next to a string label when tooltipDescription is set', () => {
    const { container } = renderWithTheme(
      <InputBase
        label="Name"
        tooltipDescription="Helper text"
        value=""
        onChange={() => {}}
      />,
    );
    expect(container.querySelector('svg')).not.toBeNull();
  });

  it('does not show the actions toolbar without actions.enabled', () => {
    const { queryByRole } = renderWithTheme(
      <InputBase
        label="Name"
        value="hello"
        onChange={() => {}}
        actions={{ forceShow: true }}
      />,
    );
    expect(queryByRole('button')).toBeNull();
  });

  it('shows the actions toolbar when forced (no real hover needed)', () => {
    const { getByRole } = renderWithTheme(
      <InputBase
        label="Name"
        value="hello"
        onChange={() => {}}
        actions={{ enabled: true, forceShow: true }}
      />,
    );
    expect(getByRole('button', { name: 'Copy to clipboard' })).toBeInTheDocument();
  });

  it('shows the actions toolbar on real mouse hover', async () => {
    const user = userEvent.setup();
    const { getByRole, queryByRole, container } = renderWithTheme(
      <InputBase
        label="Name"
        value="hello"
        onChange={() => {}}
        actions={{ enabled: true }}
      />,
    );
    expect(queryByRole('button')).toBeNull();
    // eslint-disable-next-line @typescript-eslint/no-non-null-assertion
    await user.hover(container.firstElementChild!);
    expect(getByRole('button', { name: 'Copy to clipboard' })).toBeInTheDocument();
  });

  describe('copy action', () => {
    afterEach(() => {
      vi.restoreAllMocks();
    });

    /**
     * `userEvent.setup()` unconditionally installs its own
     * `navigator.clipboard` stub (`attachClipboardStubToView`, called from
     * `setup.js`) — a real `EventTarget`-based object, not `undefined` and
     * not something a plain `Object.defineProperty(navigator, 'clipboard',
     * ...)` survives (verified: writing the mock BEFORE `userEvent.setup()`
     * gets silently clobbered the moment `setup()` runs, so the button
     * click that follows calls userEvent's stub, not the test's mock —
     * `writeText` reports zero calls despite the click firing correctly).
     * Spying on the already-installed stub's own `writeText` method, AFTER
     * `setup()`, works with the framework instead of fighting it.
     */
    function spyOnClipboardWriteText() {
      return vi.spyOn(navigator.clipboard, 'writeText');
    }

    it('writes the current value to the clipboard and reports success via onCopy', async () => {
      const user = userEvent.setup();
      const writeText = spyOnClipboardWriteText().mockResolvedValue(undefined);
      const onCopy = vi.fn();
      const { getByRole } = renderWithTheme(
        <InputBase
          label="Name"
          value="hello world"
          onChange={() => {}}
          actions={{ enabled: true, forceShow: true }}
          onCopy={onCopy}
        />,
      );
      await user.click(getByRole('button', { name: 'Copy to clipboard' }));
      expect(writeText).toHaveBeenCalledWith('hello world');
      await vi.waitFor(() => {
        expect(onCopy).toHaveBeenCalledWith('hello world');
      });
    });

    it('reports the error via onCopy when the clipboard write rejects', async () => {
      const user = userEvent.setup();
      const failure = new Error('denied');
      spyOnClipboardWriteText().mockRejectedValue(failure);
      const onCopy = vi.fn();
      const { getByRole } = renderWithTheme(
        <InputBase
          label="Name"
          value="hello"
          onChange={() => {}}
          actions={{ enabled: true, forceShow: true }}
          onCopy={onCopy}
        />,
      );
      await user.click(getByRole('button', { name: 'Copy to clipboard' }));
      await vi.waitFor(() => {
        expect(onCopy).toHaveBeenCalledTimes(1);
      });
      // `.toBe` (reference equality), not `toHaveBeenCalledWith` — the
      // rejection reason IS `failure` itself (no cloning/serialisation
      // happens on the `.then(_, onRejected)` path), and asserting via
      // reference is exact where a deep-equality matcher would also have
      // to account for `Error.stack`, which differs run to run.
      expect(onCopy.mock.calls[0]?.[0]).toBe('hello');
      expect(onCopy.mock.calls[0]?.[1]).toBe(failure);
    });
  });

  it('toggles between minRows and maxRows when the expand button is clicked', async () => {
    const user = userEvent.setup();
    const { getByRole } = renderWithTheme(
      <InputBase
        label="Description"
        value="hello"
        onChange={() => {}}
        expand={{ minRows: 2, maxRows: 8, collapsed: true }}
        actions={{ enabled: true, forceShow: true }}
      />,
    );
    expect(getByRole('button', { name: 'Expand field' })).toBeInTheDocument();
    await user.click(getByRole('button', { name: 'Expand field' }));
    expect(getByRole('button', { name: 'Collapse field' })).toBeInTheDocument();
    await user.click(getByRole('button', { name: 'Collapse field' }));
    expect(getByRole('button', { name: 'Expand field' })).toBeInTheDocument();
  });

  it('defaults the collapsed row count to 3 when expand.minRows is not given', async () => {
    // Exercises the `expand.minRows ?? 3` fallback (both the initial-state
    // computation and the toggle callback's own copy of it) — MUI's
    // autosizing textarea manages height via inline style, not an
    // observable `rows` attribute, so this asserts on behaviour (the field
    // stays interactive across the round trip) rather than a row count.
    const user = userEvent.setup();
    const { getByRole, getByLabelText } = renderWithTheme(
      <InputBase
        label="Description"
        value="hello"
        onChange={() => {}}
        expand={{ maxRows: 8, collapsed: true }}
        actions={{ enabled: true, forceShow: true }}
      />,
    );
    expect(getByRole('button', { name: 'Expand field' })).toBeInTheDocument();
    await user.click(getByRole('button', { name: 'Expand field' }));
    expect(getByRole('button', { name: 'Collapse field' })).toBeInTheDocument();
    await user.click(getByRole('button', { name: 'Collapse field' }));
    expect(getByRole('button', { name: 'Expand field' })).toBeInTheDocument();
    expect(getByLabelText('Description')).toBeInTheDocument();
  });

  it('positions the actions toolbar differently when there is no label', () => {
    const { getByRole } = renderWithTheme(
      <InputBase
        value="hello"
        onChange={() => {}}
        actions={{ enabled: true, forceShow: true }}
      />,
    );
    // No accessible name assertion needed here — the label-less field itself
    // is a separate, pre-existing a11y concern (`shared/ui`'s TextField
    // passthrough already lets a caller supply `slotProps.htmlInput['aria-label']`
    // for exactly this case); this test only exercises the toolbar's
    // hasLabel=false positioning branch.
    expect(getByRole('button', { name: 'Copy to clipboard' })).toBeInTheDocument();
  });

  it('calls onFullScreen when the full-screen action is clicked', async () => {
    const user = userEvent.setup();
    const onFullScreen = vi.fn();
    const { getByRole } = renderWithTheme(
      <InputBase
        label="Name"
        value="hello"
        onChange={() => {}}
        actions={{ enabled: true, forceShow: true }}
        onFullScreen={onFullScreen}
      />,
    );
    await user.click(getByRole('button', { name: 'Full screen view' }));
    expect(onFullScreen).toHaveBeenCalledTimes(1);
  });

  it('does not offer the expand action when actions.showExpand is false', () => {
    const { queryByRole } = renderWithTheme(
      <InputBase
        label="Name"
        value="hello"
        onChange={() => {}}
        expand={{ minRows: 2, maxRows: 8 }}
        actions={{ enabled: true, forceShow: true, showExpand: false }}
      />,
    );
    expect(queryByRole('button', { name: 'Expand field' })).toBeNull();
  });

  it('keeps the outlined label link when the caller passes its own slotProps', () => {
    const { getByLabelText } = renderWithTheme(
      <InputBase
        outlined
        label="Instructions"
        value=""
        onChange={() => {}}
        slotProps={{ inputLabel: { shrink: true }, htmlInput: { maxLength: 40 } }}
      />,
    );
    const field = getByLabelText('Instructions');
    expect(field).toHaveAttribute('aria-labelledby');
    expect(field).toHaveAttribute('maxlength', '40');
  });
});
