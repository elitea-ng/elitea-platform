import type { ReactNode } from 'react';

import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import * as artifactsFeature from '@/features/artifacts';

const mocks = vi.hoisted(() => ({
  createBucket: vi.fn(),
  editBucket: vi.fn(),
}));

import { PageChromeProvider, useShellCrumbs } from '@/shared/ui/page-chrome';

import { CreateBucket, parseRetentionDays, validateBucketName, validateRetentionDays } from './CreateBucket';
import { renderArtifactsRoute } from './__tests__/testRouter';

function CrumbProbe(): ReactNode {
  return <div data-testid="crumbs">{useShellCrumbs().map((crumb) => crumb.label).join(' / ')}</div>;
}

describe('validateRetentionDays / parseRetentionDays', () => {
  it('treats an empty box as "keep indefinitely" rather than an error', () => {
    expect(validateRetentionDays('')).toBe('');
    expect(validateRetentionDays('   ')).toBe('');
    expect(parseRetentionDays('')).toBeNull();
  });

  it('rejects non-integers and values below one day', () => {
    expect(validateRetentionDays('abc')).toContain('whole number');
    expect(validateRetentionDays('1.5')).toContain('whole number');
    expect(validateRetentionDays('0')).toContain('greater than 0');
    expect(validateRetentionDays('30')).toBe('');
    expect(parseRetentionDays(' 30 ')).toBe(30);
  });
});

describe('validateBucketName', () => {
  it('accepts supported names and rejects invalid names', () => {
    expect(validateBucketName('reports-2026')).toBe('');
    expect(validateBucketName('')).toBe('Name is required.');
    expect(validateBucketName('1reports')).toContain('Start with a letter');
    expect(validateBucketName('Reports')).toContain('lowercase');
    expect(validateBucketName('a')).toContain('at least 2');
    // The server's rule is ^[a-z][a-z0-9-]{1,62}$: 63 characters is the last accepted length.
    expect(validateBucketName(`a${'b'.repeat(62)}`)).toBe('');
    expect(validateBucketName(`a${'b'.repeat(63)}`)).toContain('63 characters');
  });
});

describe('CreateBucket', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    mocks.createBucket.mockReset().mockResolvedValue(undefined);
    mocks.editBucket.mockReset().mockResolvedValue(undefined);
    vi.spyOn(artifactsFeature, 'useArtifactMutations').mockReturnValue({
      createBucket: {
        mutateAsync: mocks.createBucket,
        isPending: false,
      },
      editBucketRetention: {
        mutateAsync: mocks.editBucket,
        isPending: false,
      },
    } as never);
    vi.spyOn(artifactsFeature, 'useArtifactBuckets').mockReturnValue({
      data: [{ id: 'docs', name: 'docs', isPinned: false, createdAt: '2026-01-01T00:00:00Z', retentionDays: 30 }],
    } as never);
  });

  it('names itself "New bucket" in the shell title row and draws no heading of its own', async () => {
    renderArtifactsRoute(
      <PageChromeProvider>
        <CreateBucket />
        <CrumbProbe />
      </PageChromeProvider>,
      '/artifacts/create-bucket',
    );
    await waitFor(() => expect(screen.getByTestId('crumbs')).toHaveTextContent('New bucket'));
    expect(screen.queryByRole('heading', { level: 1 })).toBeNull();
  });

  it('creates a valid bucket and navigates back to its artifact list', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket');
    const input = await screen.findByLabelText('Name');
    await user.clear(input);
    await user.type(input, 'reports');
    await user.click(screen.getByRole('button', { name: 'Create bucket' }));
    expect(mocks.createBucket).toHaveBeenCalledWith({ name: 'reports', retentionDays: null });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/artifacts');
      expect(router.state.location.search).toMatchObject({ bucket: 'reports' });
    });
  });

  it('lowercases the typed name and sends the retention window on create (issues 2002 and 2018)', async () => {
    const user = userEvent.setup();
    renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket');
    const input = await screen.findByLabelText('Name');
    await user.clear(input);
    await user.type(input, 'TestBucket');
    expect(input).toHaveValue('testbucket');
    await user.type(screen.getByLabelText('Retention (days)'), '14');
    await user.click(screen.getByRole('button', { name: 'Create bucket' }));
    expect(mocks.createBucket).toHaveBeenCalledWith({ name: 'testbucket', retentionDays: 14 });
  });

  /* #2018 — an empty retention sends nothing, so the project's default applies; the help must say so */
  it('says an empty retention uses the project default, and shows the server refusal', async () => {
    const user = userEvent.setup();
    renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket');
    expect(await screen.findByText("Leave empty to use the project's default retention.")).toBeInTheDocument();
    mocks.createBucket.mockRejectedValueOnce(
      Object.assign(new Error("retention_days exceeds the project's policy ceiling"), { name: 'ServerRefusal' }),
    );
    await user.type(screen.getByLabelText('Retention (days)'), '999');
    await user.click(screen.getByRole('button', { name: 'Create bucket' }));
    expect(await screen.findByRole('alert')).toHaveTextContent("retention_days exceeds the project's policy ceiling");
  });

  it('keeps invalid input local and supports cancellation', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket');
    const input = await screen.findByLabelText('Name');
    await user.clear(input);
    await user.type(input, 'invalid name');
    await user.tab();
    expect(screen.getByText(/Start with a letter/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Create bucket' })).toBeDisabled();
    expect(mocks.createBucket).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/artifacts'));
  });

  it('shows a safe message when creation fails', async () => {
    const user = userEvent.setup();
    mocks.createBucket.mockRejectedValueOnce(new Error('internal failure'));
    renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket');
    const input = await screen.findByLabelText('Name');
    await user.clear(input);
    await user.type(input, 'reports');
    await user.click(screen.getByRole('button', { name: 'Create bucket' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to create the bucket.');
  });

  it('does not submit without a selected project', async () => {
    const user = userEvent.setup();
    renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket', null);
    await user.click(await screen.findByRole('button', { name: 'Create bucket' }));
    expect(mocks.createBucket).not.toHaveBeenCalled();
  });
});

/**
 * EDIT MODE — reached the way the baseline reaches it (`Buckets.jsx:118-130`
 * navigates to the CREATE-bucket path with the bucket carried alongside),
 * not through the phantom `/artifacts/edit-bucket` the baseline declares and
 * never mounts.
 */
describe('CreateBucket in edit mode', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    mocks.createBucket.mockReset().mockResolvedValue(undefined);
    mocks.editBucket.mockReset().mockResolvedValue(undefined);
    vi.spyOn(artifactsFeature, 'useArtifactMutations').mockReturnValue({
      createBucket: { mutateAsync: mocks.createBucket, isPending: false },
      editBucketRetention: { mutateAsync: mocks.editBucket, isPending: false },
    } as never);
    vi.spyOn(artifactsFeature, 'useArtifactBuckets').mockReturnValue({
      data: [{ id: 'docs', name: 'docs', isPinned: false, createdAt: '2026-01-01T00:00:00Z', retentionDays: 30 }],
    } as never);
  });

  it('seeds the form from the bucket and saves a new retention window', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(
      <PageChromeProvider>
        <CreateBucket />
        <CrumbProbe />
      </PageChromeProvider>,
      '/artifacts/create-bucket?bucket=docs',
    );
    expect(await screen.findByTestId('crumbs')).toHaveTextContent('Edit bucket');
    expect(screen.queryByRole('heading', { level: 1 })).toBeNull();
    const retention = await screen.findByLabelText('Retention (days)');
    await waitFor(() => expect(retention).toHaveValue('30'));
    await user.clear(retention);
    await user.type(retention, '90');
    await user.click(screen.getByRole('button', { name: 'Save bucket' }));
    expect(mocks.editBucket).toHaveBeenCalledWith({ name: 'docs', retentionDays: 90 });
    expect(mocks.createBucket).not.toHaveBeenCalled();
    await waitFor(() => expect(router.state.location.pathname).toBe('/artifacts'));
  });

  it('clears the retention window to null when the box is emptied', async () => {
    const user = userEvent.setup();
    renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket?bucket=docs');
    const retention = await screen.findByLabelText('Retention (days)');
    await waitFor(() => expect(retention).toHaveValue('30'));
    await user.clear(retention);
    await user.click(screen.getByRole('button', { name: 'Save bucket' }));
    expect(mocks.editBucket).toHaveBeenCalledWith({ name: 'docs', retentionDays: null });
  });

  /**
   * The name box must not pretend to be editable: no API renames a bucket,
   * so an editable field would silently discard what the user typed.
   */
  it('shows the name read-only and never sends it', async () => {
    renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket?bucket=docs');
    const nameField = await screen.findByLabelText('Name');
    expect(nameField).toHaveValue('docs');
    expect(nameField).toHaveAttribute('readonly');
    expect(screen.getByText('A bucket cannot be renamed after it is created.')).toBeInTheDocument();
  });

  it('refuses to save an invalid retention value', async () => {
    const user = userEvent.setup();
    renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket?bucket=docs');
    const retention = await screen.findByLabelText('Retention (days)');
    await waitFor(() => expect(retention).toHaveValue('30'));
    await user.clear(retention);
    await user.type(retention, '0');
    expect(screen.getByText('Retention must be greater than 0.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save bucket' })).toBeDisabled();
    expect(mocks.editBucket).not.toHaveBeenCalled();
  });

  it('shows a safe message when the update fails', async () => {
    const user = userEvent.setup();
    mocks.editBucket.mockRejectedValueOnce(new Error('internal failure'));
    renderArtifactsRoute(<CreateBucket />, '/artifacts/create-bucket?bucket=docs');
    await screen.findByLabelText('Retention (days)');
    await user.click(screen.getByRole('button', { name: 'Save bucket' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to update the bucket.');
  });
});
