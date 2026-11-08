/** What the pane says for each refusal, end and notice of the view stream. */
import type { BrowserViewFailure } from '@/shared/api/browserView/client';
import { t } from '@/shared/i18n';

export function describeFailure(failure: BrowserViewFailure): string {
  switch (failure) {
    case 'not_found':
      return t('cloudBrowser.failure.notFound', 'No browser to show yet. It appears when the agent opens a page.');
    case 'ended':
      return t('cloudBrowser.failure.ended', 'The browser session has ended.');
    case 'busy':
      return t('cloudBrowser.failure.busy', 'The browser service is busy. Trying again…');
    case 'access_revoked':
      return t('cloudBrowser.failure.accessRevoked', 'You can no longer see this browser.');
    case 'signed_out':
      return t('cloudBrowser.failure.signedOut', 'Sign in again to see the browser.');
    case 'forbidden':
      return t('cloudBrowser.failure.forbidden', 'This page cannot open the browser view. Open Elitea at its own address.');
    case 'disconnected':
      return t('cloudBrowser.failure.disconnected', 'The connection to the browser dropped. Reconnecting…');
    case 'upgrade_required':
      return t('cloudBrowser.failure.upgradeRequired', 'Update the app to see the browser: this version is older than the workspace allows.');
    case 'unavailable':
    default:
      return t('cloudBrowser.failure.unavailable', 'The browser is not available right now. Trying again…');
  }
}

export function describeEnd(code: string | null): string {
  switch (code ?? '') {
    case 'idle':
      return t('cloudBrowser.end.idle', 'The browser closed after a while without use.');
    case 'wall':
      return t('cloudBrowser.end.wall', 'The browser reached its time limit and closed.');
    case 'agent_failed':
      return t('cloudBrowser.end.agentFailed', 'The browser stopped unexpectedly.');
    default:
      return t('cloudBrowser.end.closed', 'The browser session has ended.');
  }
}

export function describeNotice(code: string | null): string | null {
  switch (code ?? '') {
    case 'control_revoked':
      return t('cloudBrowser.notice.controlRevoked', 'You can no longer take over this browser. The agent has it back.');
    case 'input_refused':
      return t('cloudBrowser.notice.inputRefused', 'The browser did not take that input.');
    case 'engine_switched':
      return t('cloudBrowser.notice.engineSwitched', 'The page needed the full browser, so it was reopened there.');
    case 'malformed':
      return null;
    default:
      return null;
  }
}
