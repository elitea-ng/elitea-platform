/**
 * The composer's attach control, in all three of the forms it renders.
 *
 * It had no test file of its own before this one, which is part of why the
 * chat composer could take a file and show nothing: every assertion about
 * attachments lived either below this component (`useAttachmentState`,
 * `validateAttachmentFiles`) or above it (`PlusChatButton`'s drop handle), and
 * the control itself — the counter, the disabled ceiling, the rejection
 * message, and the shape it takes when it is only a drop target — was covered
 * by neither.
 */
import { createRef } from 'react';
import type { ReactNode } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { ATTACHMENT_LIMITS } from '@/shared/lib/attachments';

import { server } from '../../../../test/setup';
import { AttachmentButton } from './AttachmentButton';
import type { AttachmentButtonHandle, AttachmentButtonProps } from './AttachmentButton';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const BASE = '/api/v2';
const PROJECT = 'p1';

afterEach(() => {
  resetGeneratedClient();
});

/**
 * The served allow-list this control now reads (#940 A15). Every render below
 * answers `index_types`, because an UNANSWERED request is not neutral here:
 * `eliteaFetch` reports it as a 200 with no maps, which is a deployment
 * saying "nothing is allowed" — the disabled state ELITEA-0489 is about. A
 * test that left it unanswered would be measuring that state by accident.
 */
function serveAllowedTypes(body: Record<string, unknown> | undefined): void {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/index_types/prompt_lib/${PROJECT}`, () =>
      HttpResponse.json(body ?? {
        items: [], total: 0,
        document_types: { '.txt': 'text/plain', '.pdf': 'application/pdf' },
        image_types: { '.png': 'image/png' },
        code_types: { '.sql': 'text/x-sql', '.sh': 'text/x-shellscript' },
      }),
    ),
  );
}

/**
 * A REAL router and a REAL query client, because the control resolves the
 * selected project through `useRouteContext` and reads the allow-list through
 * TanStack Query. `strict: false` degrades a no-MATCH lookup, not a wholly
 * absent router (`VoicePersonalizationSection.test.tsx` records the same
 * finding).
 */
async function renderButton(props: AttachmentButtonProps & { readonly handleRef?: ReturnType<typeof createRef<AttachmentButtonHandle>> } = {}) {
  const { handleRef, ...rest } = props;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });

  function RootComponent(): ReactNode {
    return (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          {/* The sentinel below is what `renderButton` waits for. RouterProvider
            * mounts ASYNCHRONOUSLY, so a test that interacted straight after
            * `render` found an empty document and read it as "the control does
            * nothing" — seven of this file's eight tests, all at once. */}
          <div data-testid="attachment-button-host">
            <AttachmentButton
              ref={handleRef}
              {...rest}
            />
          </div>
        </ThemeProvider>
      </QueryClientProvider>
    );
  }

  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => PROJECT } },
  });
  const result = render(<RouterProvider router={router} />);
  await screen.findByTestId('attachment-button-host');
  return result;
}

function picker(): HTMLInputElement {
  const input = document.querySelector<HTMLInputElement>('input[type="file"]');
  if (input === null) throw new Error('Expected the hidden file input');
  return input;
}

/** `input.files` is read-only in jsdom, so the picked set is installed directly. */
function pick(files: readonly File[]): void {
  const input = picker();
  Object.defineProperty(input, 'files', { configurable: true, value: files });
  fireEvent.change(input);
}

beforeEach(() => {
  serveAllowedTypes(undefined);
});

describe('AttachmentButton — the icon form', () => {
  it('opens the picker and reports the accepted files', async () => {
    const user = userEvent.setup();
    const onAttachFiles = vi.fn();
    await renderButton({ onAttachFiles });

    const opened = vi.spyOn(picker(), 'click');
    await user.click(screen.getByRole('button', { name: 'attach files' }));
    expect(opened).toHaveBeenCalledTimes(1);

    const file = new File(['x'], 'notes.txt', { type: 'text/plain' });
    pick([file]);
    expect(onAttachFiles).toHaveBeenCalledWith([file]);
    // Reset, so re-picking the SAME file fires `change` again.
    expect(picker().value).toBe('');
  });

  it('closes at the file ceiling — button, input and picker all', async () => {
    const onAttachFiles = vi.fn();
    const full = Array.from(
      { length: ATTACHMENT_LIMITS.MAX_ATTACHMENTS },
      (_unused, index) => new File(['x'], `f${index}.txt`),
    );
    await renderButton({ attachments: full, onAttachFiles });

    // Both halves matter: a disabled BUTTON with a live input still takes a
    // drop, and a live button with a disabled input opens a picker that
    // cannot answer.
    expect(screen.getByRole('button', { name: 'attach files' })).toBeDisabled();
    expect(picker()).toBeDisabled();

    fireEvent.click(screen.getByRole('button', { name: 'attach files' }));
    expect(onAttachFiles).not.toHaveBeenCalled();
  });

  it('drops the files the validator rejects and reports them', async () => {
    const onAttachFiles = vi.fn();
    const onError = vi.fn();
    await renderButton({ onAttachFiles, onError });

    const tooBig = new File([], 'huge.png', { type: 'image/png' });
    Object.defineProperty(tooBig, 'size', { value: ATTACHMENT_LIMITS.MAX_IMAGE_FILE_SIZE + 1 });
    pick([tooBig]);

    expect(onAttachFiles).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledTimes(1);
    expect(String(onError.mock.calls[0]?.[0])).toContain('huge.png');
  });

  it('ignores an empty pick', async () => {
    const onAttachFiles = vi.fn();
    const onError = vi.fn();
    await renderButton({ onAttachFiles, onError });

    pick([]);

    expect(onAttachFiles).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });
});

describe('AttachmentButton — the menu-row form', () => {
  it('shows the remaining capacity as text and activates from the keyboard', async () => {
    const user = userEvent.setup();
    const onAttachFiles = vi.fn();
    await renderButton({ showLabel: true, attachments: [new File(['x'], 'one.txt')], onAttachFiles });

    const row = screen.getByTestId('plus-menu-attachments');
    expect(row).toHaveTextContent(`${ATTACHMENT_LIMITS.MAX_ATTACHMENTS - 1} left`);

    const opened = vi.spyOn(picker(), 'click');
    row.focus();
    await user.keyboard('{Enter}');
    expect(opened).toHaveBeenCalledTimes(1);
  });

  it('goes aria-disabled at the ceiling and opens nothing', async () => {
    const onError = vi.fn();
    const full = Array.from(
      { length: ATTACHMENT_LIMITS.MAX_ATTACHMENTS },
      (_unused, index) => new File(['x'], `f${index}.txt`),
    );
    await renderButton({ showLabel: true, attachments: full, onError });

    const row = screen.getByTestId('plus-menu-attachments');
    expect(row).toHaveAttribute('aria-disabled', 'true');
    expect(row).toHaveTextContent('0 left');
    // A `menuitem` is a div, so `aria-disabled` alone stops no click: the
    // component withholds the handler as well, and it is out of the tab order.
    expect(row).toHaveAttribute('tabindex', '-1');

    const opened = vi.spyOn(picker(), 'click');
    fireEvent.click(row);
    fireEvent.keyDown(row, { key: 'Enter' });
    expect(opened).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });
});

describe('AttachmentButton — the drop-target-only form', () => {
  it('renders no control and no input, and still serves the handle', async () => {
    const handleRef = createRef<AttachmentButtonHandle>();
    const onAttachFiles = vi.fn();
    const { container } = await renderButton({ dropTargetOnly: true, handleRef, onAttachFiles });

    // The HOST is empty, not the whole container: the router's own wrapper
    // and this file's mount sentinel live above it (see `renderButton`).
    expect(screen.getByTestId('attachment-button-host')).toBeEmptyDOMElement();
    expect(container.querySelector('input[type="file"]')).toBeNull();
    expect(screen.queryByRole('button', { name: 'attach files' })).toBeNull();

    const file = new File(['x'], 'dropped.txt');
    handleRef.current?.onDrop({ dataTransfer: { files: [file] }, preventDefault: () => {} });
    expect(onAttachFiles).toHaveBeenCalledWith([file]);
  });

  it('ignores a drop while attachments are disabled', async () => {
    const handleRef = createRef<AttachmentButtonHandle>();
    const onAttachFiles = vi.fn();
    await renderButton({ dropTargetOnly: true, handleRef, onAttachFiles, disableAttachments: true });

    handleRef.current?.onDrop({ dataTransfer: { files: [new File(['x'], 'dropped.txt')] }, preventDefault: () => {} });
    expect(onAttachFiles).not.toHaveBeenCalled();
  });
});

/**
 * The backend-served allow-list (#940 A15).
 *
 * Every case here is about one distinction the control has to hold: a list
 * that was SERVED and a list that could not be. They look identical in an
 * extension array and they mean opposite things — "reject everything" and
 * "the deployment cannot say, carry on".
 */
describe('AttachmentButton — the served file-type allow-list', () => {
  /* ELITEA-0484/0486: a code file whose extension IS in the served code_types
   * is accepted, alongside a document. */
  it('accepts a code file and a document that the served list allows', async () => {
    const onAttachFiles = vi.fn();
    const onError = vi.fn();
    await renderButton({ onAttachFiles, onError });
    // The query settles from `isLoading` (defaults) to the served list.
    await waitFor(() => expect(picker().accept).toContain('.sql'));

    const query = new File(['select 1'], 'report.sql', { type: 'text/x-sql' });
    const brief = new File(['hi'], 'brief.txt', { type: 'text/plain' });
    pick([query, brief]);

    expect(onAttachFiles).toHaveBeenCalledWith([query, brief]);
    expect(onError).not.toHaveBeenCalled();
  });

  /* ELITEA-0488: an extension the backend does not list is rejected, with a
   * message that says it is the TYPE and not the size. */
  it('rejects a file whose extension the served list does not carry', async () => {
    const onAttachFiles = vi.fn();
    const onError = vi.fn();
    await renderButton({ onAttachFiles, onError });
    await waitFor(() => expect(picker().accept).toContain('.sql'));

    pick([new File(['MZ'], 'installer.exe', { type: 'application/octet-stream' })]);

    expect(onAttachFiles).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledWith(expect.stringContaining('not a supported file type'));
    expect(onError).toHaveBeenCalledWith(expect.stringContaining('installer.exe'));
  });

  /* ELITEA-0487: the served list does not regress what already worked — an
   * image and a document still attach. */
  it('still accepts images and documents once the list is served', async () => {
    const onAttachFiles = vi.fn();
    await renderButton({ onAttachFiles });
    await waitFor(() => expect(picker().accept).toContain('.png'));

    const image = new File(['\x89PNG'], 'diagram.png', { type: 'image/png' });
    const document_ = new File(['%PDF'], 'spec.pdf', { type: 'application/pdf' });
    pick([image, document_]);

    expect(onAttachFiles).toHaveBeenCalledWith([image, document_]);
  });

  /* ELITEA-0489: a deployment that SERVES an empty list turns the control off
   * — the picker cannot even be opened. */
  it('disables the control when the deployment serves no file types', async () => {
    serveAllowedTypes({ items: [], total: 0, document_types: {}, image_types: {}, code_types: {} });
    const onAttachFiles = vi.fn();
    await renderButton({ onAttachFiles });

    await waitFor(() => expect(screen.getByRole('button', { name: 'attach files' })).toBeDisabled());
    expect(picker().disabled).toBe(true);
  });

  /* The other half of that distinction, and the reason `isServed` exists: a
   * deployment that cannot enumerate loaders (501, ELITEA_INDEX_TYPES_ENABLED
   * off) keeps the control live on the defaults. Reading that as "no types"
   * would silently disable attachments for every such installation. */
  it('stays live on the defaults when the deployment cannot enumerate loaders', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/elitea_core/index_types/prompt_lib/${PROJECT}`, () =>
        HttpResponse.json({ error: 'not available', code: 'index_types_not_available' }, { status: 501 }),
      ),
    );
    const onAttachFiles = vi.fn();
    const onError = vi.fn();
    await renderButton({ onAttachFiles, onError });

    expect(screen.getByRole('button', { name: 'attach files' })).toBeEnabled();
    const brief = new File(['hi'], 'brief.txt', { type: 'text/plain' });
    pick([brief]);
    expect(onAttachFiles).toHaveBeenCalledWith([brief]);
    expect(onError).not.toHaveBeenCalled();
  });
});
