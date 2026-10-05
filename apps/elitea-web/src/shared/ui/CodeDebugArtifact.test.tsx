import { Blob as NodeBlob } from 'node:buffer';
import { webcrypto } from 'node:crypto';
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it, vi, type MockInstance } from 'vitest';

import { resetConfigForTests } from '@/shared/config/get-config';
import fixture from '@/shared/lib/fixtures/code-debug-public-trace.json';
import snapshot from '@/shared/lib/fixtures/code-debug-snapshot.json?raw';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';
import { CodeDebugArtifact, CodeDebugTraceArtifact } from './CodeDebugArtifact';

const first = fixture.first_attempt_history.metadata.code_debug_v1;
const retry = fixture.retry_committed.metadata.code_debug_v1;
const downloadName = 'Download verified snapshot';
const route = '/api/v2/artifacts/objects/7/code-debug/:name';
let createUrl: MockInstance<typeof URL.createObjectURL>;
let revokeUrl: MockInstance<typeof URL.revokeObjectURL>;
let anchorClick: MockInstance<() => void>;

beforeEach(() => {
  vi.stubGlobal('Blob', NodeBlob);
  vi.stubGlobal('crypto', webcrypto);
  vi.stubGlobal('elitea_ui_config', { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public', allow_project_own_llms: false });
  resetConfigForTests();
  createUrl = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:verified-code-debug');
  revokeUrl = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  anchorClick = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
});
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); resetConfigForTests(); });

function node(value: unknown = first, scopeKey = 'conversation-1', projectId: string | undefined = '7') {
  return <CodeDebugArtifact value={value} projectId={projectId} scopeKey={scopeKey} />;
}

it('reads only on an explicit click and downloads the verified raw snapshot once', async () => {
  let calls = 0;
  server.use(http.get(route, ({ params }) => {
    expect(params['name']).toBe(first.artifact.name);
    calls++;
    return new HttpResponse(snapshot, { headers: { 'Content-Type': 'application/json' } });
  }));
  renderWithTheme(node());
  expect(screen.getByText('Code debug · run · attempt 1')).toBeVisible();
  expect(calls).toBe(0);
  await userEvent.click(screen.getByRole('button', { name: downloadName }));
  await waitFor(() => expect(createUrl).toHaveBeenCalledTimes(1));
  expect(calls).toBe(1);
  expect(anchorClick).toHaveBeenCalledTimes(1);
  expect(revokeUrl).toHaveBeenCalledWith('blob:verified-code-debug');
  const blob = createUrl.mock.calls[0]?.[0];
  if (!(blob instanceof Blob)) throw new Error('Expected a verified Blob');
  expect(await blob.text()).toBe(snapshot);
});

it.each(['denied', 'unavailable'])('shows %s guidance without an artifact read or link', status => {
  const value = { ...fixture.retry_current_warning.metadata.code_debug_v1, status };
  renderWithTheme(node(value));
  expect(screen.getByText('Code debug · run · attempt 2')).toBeVisible();
  expect(screen.getByRole('status')).toHaveTextContent('Code execution continues.');
  expect(screen.queryByRole('button', { name: downloadName })).toBeNull();
  expect(screen.queryByRole('link')).toBeNull();
});

it.each([null, { ...first, grant: 'not authority' }, { ...first, artifact: { ...first.artifact, url: 'https://example.invalid' } }])('keeps an invalid proof inert', value => {
  renderWithTheme(node(value));
  expect(screen.getByRole('status')).toHaveTextContent('reference is invalid');
  expect(screen.queryByRole('button', { name: downloadName })).toBeNull();
});

it.each(['8', undefined])('uses the caller project instead of the artifact project (%s)', projectId => {
  renderWithTheme(<CodeDebugArtifact value={first} scopeKey="selected" projectId={projectId} />);
  expect(screen.getByRole('status')).toHaveTextContent('selected project');
  expect(screen.queryByRole('button', { name: downloadName })).toBeNull();
});

it('shows a denied export without requiring a download project', () => {
  renderWithTheme(<CodeDebugArtifact value={{ ...fixture.retry_current_warning.metadata.code_debug_v1, status: 'denied' }} scopeKey="selected" projectId={undefined} />);
  expect(screen.getByRole('status')).toHaveTextContent('export was denied');
  expect(screen.queryByRole('button', { name: downloadName })).toBeNull();
});

it('does not read conflicting trace copies', () => {
  const attrs = structuredClone(fixture.first_attempt_history);
  attrs.tool_meta.metadata.code_debug_v1.original_visit.visit_id = retry.original_visit.visit_id;
  renderWithTheme(<CodeDebugTraceArtifact attrs={attrs} projectId="7" scopeKey="trace-1" />);
  expect(screen.getByRole('status')).toHaveTextContent('reference is invalid');
});

it.each([401, 403])('does not expose private HTTP %s bodies', async status => {
  server.use(http.get(route, () => HttpResponse.json({ error: 'PRIVATE_SOURCE_CREDENTIAL' }, { status })));
  renderWithTheme(node());
  await userEvent.click(screen.getByRole('button', { name: downloadName }));
  expect(await screen.findByRole('alert')).toHaveTextContent('Access is denied.');
  expect(screen.queryByText(/PRIVATE_SOURCE_CREDENTIAL/)).toBeNull();
  expect(createUrl).not.toHaveBeenCalled();
});

it('refuses replaced storage bytes without creating a link', async () => {
  server.use(http.get(route, () => new HttpResponse(snapshot.replace('hello', 'HELLO'))));
  renderWithTheme(node());
  await userEvent.click(screen.getByRole('button', { name: downloadName }));
  expect(await screen.findByRole('alert')).toHaveTextContent('bytes do not match');
  expect(createUrl).not.toHaveBeenCalled();
});

function delayedRead() {
  let release = (): void => {};
  const gate = new Promise<void>(resolve => { release = resolve; });
  const read = vi.fn();
  server.use(http.get(route, async () => {
    read();
    await gate;
    return new HttpResponse(snapshot, { headers: { 'Content-Type': 'application/json' } });
  }));
  const fetchObservation = vi.spyOn(globalThis, 'fetch');
  return { read, release, fetchObservation };
}

it('rejects double clicks while the explicit read is pending', async () => {
  const delayed = delayedRead();
  renderWithTheme(node());
  const user = userEvent.setup();
  await user.dblClick(screen.getByRole('button', { name: downloadName }));
  await waitFor(() => expect(delayed.read).toHaveBeenCalledTimes(1));
  expect(screen.getByRole('button', { name: 'Verifying...' })).toBeDisabled();
  delayed.release();
  await waitFor(() => expect(createUrl).toHaveBeenCalledTimes(1));
});

it.each(['visit', 'scope', 'project'])('aborts an A→B→A %s read and requires a fresh click', async transition => {
  const delayed = delayedRead();
  const view = renderWithTheme(node());
  await userEvent.click(screen.getByRole('button', { name: downloadName }));
  await waitFor(() => expect(delayed.read).toHaveBeenCalledTimes(1));
  const signal = delayed.fetchObservation.mock.calls[0]?.[1]?.signal;
  view.rerender(transition === 'visit' ? node(retry) : transition === 'scope' ? node(first, 'conversation-2') : node(first, 'conversation-1', '8'));
  expect(signal?.aborted).toBe(true);
  view.rerender(node());
  const pendingRead = delayed.fetchObservation.mock.results[0]?.value as Promise<Response> | undefined;
  await act(async () => { delayed.release(); await pendingRead?.catch(() => undefined); });
  expect(createUrl).not.toHaveBeenCalled();
  expect(screen.getByRole('button', { name: downloadName })).toBeEnabled();
  await userEvent.click(screen.getByRole('button', { name: downloadName }));
  await waitFor(() => expect(createUrl).toHaveBeenCalledTimes(1));
  expect(delayed.read).toHaveBeenCalledTimes(2);
});

it('aborts on unmount and never downloads the delayed read', async () => {
  const delayed = delayedRead();
  const view = renderWithTheme(node());
  await userEvent.click(screen.getByRole('button', { name: downloadName }));
  await waitFor(() => expect(delayed.read).toHaveBeenCalledTimes(1));
  const signal = delayed.fetchObservation.mock.calls[0]?.[1]?.signal;
  view.unmount();
  expect(signal?.aborted).toBe(true);
  const pendingRead = delayed.fetchObservation.mock.results[0]?.value as Promise<Response> | undefined;
  await act(async () => { delayed.release(); await pendingRead?.catch(() => undefined); });
  expect(createUrl).not.toHaveBeenCalled();
});
