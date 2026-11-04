import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getGetPipelineInboundTriggerResponseMock } from '@/shared/api/generated/applications/applications.msw';
import { getListConversationsMockHandler } from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, EliteaApiError, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../../__tests__/testUtils';
import { PipelineWebhookModal, type PipelineWebhookModalProps } from './PipelineWebhookModal';

const URL_PATH = '/api/v2/pipeline_trigger/1/tok-abc';

function noop(): void {
  /* intentionally empty */
}

describe('PipelineWebhookModal', () => {
  it('renders nothing while closed', () => {
    const { queryByText } = renderWithTheme(
      <PipelineWebhookModal
        open={false}
        onClose={noop}
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(queryByText('Webhook settings')).toBeNull();
  });

  it('shows the inbound URL resolved against this origin', () => {
    const { getByTestId } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(getByTestId('pipeline-webhook-url')).toHaveValue(`${window.location.origin}${URL_PATH}`);
  });

  /**
   * The plain read never carries a credential — that is the backend's rule
   * (`/pipeline_triggers/secret/...` is a separate operation with the WRITE
   * permission on it), so "no secret" is a normal view of a live trigger and
   * not an error state.
   */
  it('explains where the secret is when none has been revealed', () => {
    const { queryByTestId, getByText } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(queryByTestId('pipeline-webhook-secret')).toBeNull();
    expect(getByText(/stored server-side/)).toBeInTheDocument();
  });

  it('masks a revealed secret until the eye is clicked', async () => {
    const user = userEvent.setup();
    const { getByTestId, getByLabelText } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        secretValue="s3cr3t"
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(getByTestId('pipeline-webhook-secret')).toHaveValue('••••••');
    await user.click(getByLabelText('Show secret'));
    expect(getByTestId('pipeline-webhook-secret')).toHaveValue('s3cr3t');
  });

  it('keeps the secret out of the example request until it is shown', async () => {
    const user = userEvent.setup();
    const { getByText, getByLabelText, queryByText } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        secretValue="s3cr3t"
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(getByText(/Bearer <your_secret>/)).toBeInTheDocument();
    await user.click(getByLabelText('Show secret'));
    expect(queryByText(/Bearer <your_secret>/)).toBeNull();
    expect(getByText(/Bearer s3cr3t/)).toBeInTheDocument();
  });

  it('wires reveal, rotate and revoke to their own callbacks', async () => {
    const user = userEvent.setup();
    const onReveal = vi.fn();
    const onRotate = vi.fn();
    const onRevoke = vi.fn();
    const { getByTestId } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        onReveal={onReveal}
        onRotate={onRotate}
        onRevoke={onRevoke}
      />,
    );
    await user.click(getByTestId('pipeline-webhook-reveal'));
    await user.click(getByTestId('pipeline-webhook-rotate'));
    await user.click(getByTestId('pipeline-webhook-revoke'));
    expect(onReveal).toHaveBeenCalledOnce();
    expect(onRotate).toHaveBeenCalledOnce();
    expect(onRevoke).toHaveBeenCalledOnce();
  });

  it('disables every action while a write is in flight', () => {
    const { getByTestId } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        isLoading
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(getByTestId('pipeline-webhook-reveal')).toBeDisabled();
    expect(getByTestId('pipeline-webhook-rotate')).toBeDisabled();
    expect(getByTestId('pipeline-webhook-revoke')).toBeDisabled();
  });

  /**
   * `userEvent.setup()` installs its OWN `navigator.clipboard` stub, so a
   * bespoke `vi.fn()` here would be clobbered and the assertion would
   * false-negative — read the copied text back through that stub instead, the
   * same technique `shared/ui/CopyToClipboardButton`'s own test records.
   */
  it('copies the URL and notifies', async () => {
    const onNotify = vi.fn();
    const user = userEvent.setup();
    const { getByLabelText } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
        onNotify={onNotify}
      />,
    );
    await user.click(getByLabelText('Copy URL'));
    expect(await navigator.clipboard.readText()).toBe(`${window.location.origin}${URL_PATH}`);
    expect(onNotify).toHaveBeenCalledWith('Webhook URL copied to clipboard');
  });

  /* ── #970: the two authentication modes ─────────────────────────────── */

  /**
   * A signing trigger is the one a GitHub repository webhook can actually
   * call, and the ONE thing the person configuring that webhook needs from
   * this dialog is which header to sign into. It is on the plain read, so it
   * is shown without revealing anything.
   */
  it('shows the signature header a signing trigger is configured with', () => {
    const { getByTestId } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={`${URL_PATH}/github`}
        authMode="hmac_sha256"
        signatureHeader="X-Hub-Signature-256"
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(getByTestId('pipeline-webhook-signature-header')).toHaveValue('X-Hub-Signature-256');
    expect(getByTestId('pipeline-webhook-mode-hint').textContent).toContain('X-Hub-Signature-256');
    // The example must not show the bearer form for a trigger that refuses
    // it: a copied command that always answers 401 is worse than none.
    expect(getByTestId('pipeline-webhook-url')).toHaveValue(`${window.location.origin}${URL_PATH}/github`);
  });

  /** A bearer trigger keeps the carrier documentation and shows no header. */
  it('shows no signature header for a bearer trigger', () => {
    const { queryByTestId, getByTestId } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        authMode="token"
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(queryByTestId('pipeline-webhook-signature-header')).toBeNull();
    expect(getByTestId('pipeline-webhook-mode-hint').textContent).toContain('Authorization: Bearer');
  });

  /**
   * Changing the type is not applied on selection: the backend writes the
   * mode on the create/rotate route and nowhere else, so applying it replaces
   * the credential. The dialog says so and asks for a second click.
   */
  it('asks before applying a type change, and rotates with the chosen one', async () => {
    const onRotate = vi.fn();
    const user = userEvent.setup();
    const { getByTestId, queryByTestId, getByRole } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        authMode="token"
        onReveal={noop}
        onRotate={onRotate}
        onRevoke={noop}
      />,
    );
    expect(queryByTestId('pipeline-webhook-apply-mode')).toBeNull();

    await user.click(getByRole('combobox'));
    await user.click(getByRole('option', { name: 'GitHub (signed payload)' }));

    expect(getByTestId('pipeline-webhook-mode-pending').textContent).toContain('rotates the credential');
    expect(onRotate).not.toHaveBeenCalled();

    await user.click(getByTestId('pipeline-webhook-apply-mode'));
    expect(onRotate).toHaveBeenCalledWith('github');
  });

  /** The plain Rotate button keeps the mode the row already carries. */
  it('rotates a signing trigger as a signing trigger', async () => {
    const onRotate = vi.fn();
    const user = userEvent.setup();
    const { getByTestId } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={`${URL_PATH}/github`}
        authMode="hmac_sha256"
        signatureHeader="X-Hub-Signature-256"
        onReveal={noop}
        onRotate={onRotate}
        onRevoke={noop}
      />,
    );
    await user.click(getByTestId('pipeline-webhook-rotate'));
    expect(onRotate).toHaveBeenCalledWith('github');
  });

  /* ── legacy issue 6664: GitLab ─────────────────────────────────────── */

  /** A bearer row with the `gitlab` provider is the GitLab secret-token entry, and it names GitLab's own header. */
  it('shows a GitLab secret-token trigger as GitLab, with X-Gitlab-Token', () => {
    const { getByTestId, queryByTestId, getByRole } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={`${URL_PATH}/gitlab`}
        authMode="token"
        provider="gitlab"
        secretValue="s3cret"
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(getByRole('combobox').textContent).toContain('GitLab (secret token)');
    expect(getByTestId('pipeline-webhook-mode-hint').textContent).toContain('X-Gitlab-Token');
    expect(queryByTestId('pipeline-webhook-signature-header')).toBeNull();
  });

  /** A Standard Webhooks row is the GitLab signing-token entry, and its header is fixed. */
  it('shows a GitLab signing-token trigger with the webhook-signature header', () => {
    const { getByTestId, getByRole } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={`${URL_PATH}/gitlab`}
        authMode="standard_webhooks_hmac"
        provider="gitlab"
        signatureHeader="webhook-signature"
        onReveal={noop}
        onRotate={noop}
        onRevoke={noop}
      />,
    );
    expect(getByRole('combobox').textContent).toContain('GitLab (signing token)');
    expect(getByTestId('pipeline-webhook-signature-header')).toHaveValue('webhook-signature');
    expect(getByTestId('pipeline-webhook-mode-hint').textContent).toContain('five minutes');
  });

  it.each([
    ['GitLab (secret token)', 'gitlab'],
    ['GitLab (signing token)', 'gitlab_signing'],
  ])('applies the %s type by rotating with it', async (label, mode) => {
    const onRotate = vi.fn();
    const user = userEvent.setup();
    const { getByTestId, getByRole } = renderWithTheme(
      <PipelineWebhookModal
        open
        onClose={noop}
        webhookUrl={URL_PATH}
        authMode="token"
        onReveal={noop}
        onRotate={onRotate}
        onRevoke={noop}
      />,
    );
    await user.click(getByRole('combobox'));
    await user.click(getByRole('option', { name: label }));
    await user.click(getByTestId('pipeline-webhook-apply-mode'));
    expect(onRotate).toHaveBeenCalledWith(mode);
  });
});

/** The thread of the v2.yaml trigger examples, in the cloud. */
const THREAD = '6f1c2d3e-4a5b-4c6d-8e7f-901234567890';
const LISTING = {
  total: 1,
  rows: [{ id: 11, uuid: THREAD, name: 'Alert triage', source: 'local_work', workspace_executor: 'cloud', created_at: '2026-11-04T09:00:00Z', updated_at: '2026-11-04T09:00:00Z', duration: -1, message_groups_count: 2, is_pinned: false, is_archived: false }],
};

function renderTargeted(target: PipelineWebhookModalProps['target']) {
  const onRotate = vi.fn<PipelineWebhookModalProps['onRotate']>(() => Promise.resolve(undefined));
  renderWithRouterAndProject(
    <PipelineWebhookModal open onClose={noop} webhookUrl={URL_PATH} onReveal={noop} onRotate={onRotate} onRevoke={noop} projectId="1" target={target} />,
    '1',
  );
  return { onRotate, user: userEvent.setup() };
}

describe('PipelineWebhookModal — Runs on a handed-off workspace thread (ADR-0045 decision 9)', () => {
  beforeEach(() => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListConversationsMockHandler(LISTING));
  });
  afterEach(() => resetGeneratedClient());

  it('offers the target only when it changed, and rotates with it', async () => {
    const { onRotate, user } = renderTargeted({ target_kind: 'pipeline' });
    expect(await screen.findByLabelText('This pipeline')).toBeChecked();
    expect(screen.queryByTestId('pipeline-webhook-apply-target')).toBeNull();
    await user.click(screen.getByLabelText('A handed-off workspace thread'));
    expect(screen.getByTestId('pipeline-webhook-target-pending')).toHaveTextContent('Applying this target rotates the credential');
    await user.click(await screen.findByRole('combobox', { name: 'Thread' }));
    await user.click(await screen.findByRole('option', { name: 'Alert triage' }));
    await user.type(screen.getByTestId('thread-target-prompt'), 'Triage the alert.');
    await user.click(screen.getByTestId('pipeline-webhook-apply-target'));
    expect(onRotate).toHaveBeenCalledWith('custom', { target_kind: 'workspace_thread', conversation_uuid: THREAD, prompt: 'Triage the alert.' });
  });

  it('seeds a stored thread trigger (the v2.yaml example) and sends its stored grant back with an edited message', async () => {
    const { onRotate, user } = renderTargeted(getGetPipelineInboundTriggerResponseMock());
    expect(await screen.findByLabelText('A handed-off workspace thread')).toBeChecked();
    expect(screen.getByTestId('pipeline-webhook-thread-hint')).toBeInTheDocument();
    await user.type(screen.getByTestId('thread-target-prompt'), ' Then report.');
    await user.click(screen.getByTestId('pipeline-webhook-apply-target'));
    expect(onRotate).toHaveBeenCalledWith('custom', {
      target_kind: 'workspace_thread',
      conversation_uuid: THREAD,
      prompt: 'Triage the alert in the payload and open a fix branch. Then report.',
      grant: { continue_step_limit: true, continue_output_limit: true, goal_check: true },
    });
  });

  it('clears a stored thread with target_kind pipeline', async () => {
    const { onRotate, user } = renderTargeted(getGetPipelineInboundTriggerResponseMock());
    await user.click(await screen.findByLabelText('This pipeline'));
    await user.click(screen.getByTestId('pipeline-webhook-apply-target'));
    expect(onRotate).toHaveBeenCalledWith('custom', { target_kind: 'pipeline' });
  });

  it('shows a refusal of the thread on its field', async () => {
    const { onRotate, user } = renderTargeted(getGetPipelineInboundTriggerResponseMock());
    onRotate.mockResolvedValue(new EliteaApiError({ kind: 'http', status: 404, url: '/api/v2/pipeline_triggers/prompt_lib/1/7', body: { error: 'no such conversation' } }));
    await user.type(await screen.findByTestId('thread-target-prompt'), '!');
    await user.click(screen.getByTestId('pipeline-webhook-apply-target'));
    await waitFor(() => expect(screen.getByText('This thread is not in this project, or you are not a participant of it.')).toBeInTheDocument());
  });
});
