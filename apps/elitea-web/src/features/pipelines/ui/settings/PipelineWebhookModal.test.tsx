import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { PipelineWebhookModal } from './PipelineWebhookModal';

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
});
