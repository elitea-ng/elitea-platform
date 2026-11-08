/**
 * A Local work thread's Browser tab (ADR-0048): the desktop's own pane while
 * the thread runs here (⌘⇧B opens it), and the cloud run's browser streamed
 * when it runs in the cloud (decision 7, #1592), in the same tab.
 */
import { lazy, Suspense } from 'react';

import { BrowserPane, useBrowserPane, useBrowserPanes } from '@/features/workspace-browser';
import { InspectorTab } from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';

const CloudBrowserView = lazy(() => import('@/shared/ui/CloudBrowserView').then((module) => ({ default: module.CloudBrowserView })));

/** Browser (⌘⇧B, ADR-0048): offered while the person has the thread's pane open; it fills the tab, unpadded. */
function BrowserTab({ threadId, subject }: { readonly threadId: string; readonly subject: string }): React.JSX.Element | null {
  const open = useBrowserPane(threadId).open;
  if (!open) return null;
  return (
    <InspectorTab id="browser" title={t('workspace.panel.browser', 'Browser')} subject={subject}>
      <BrowserPane threadId={threadId} onClose={() => useBrowserPanes.getState().update(threadId, { open: false })} />
    </InspectorTab>
  );
}

/**
 * Browser, for a cloud thread (ADR-0048 decision 7, #1592): the same tab as
 * the local pane, showing the cloud run's browser streamed (the toolbar, the
 * pill, take over and hand back as in the web chat). Offered once the run
 * used the browser; the host opens the view's socket with the bearer.
 */
function CloudBrowserTab({ sessionId, subject }: { readonly sessionId: string; readonly subject: string }): React.JSX.Element {
  return (
    <InspectorTab id="browser" title={t('workspace.panel.browser', 'Browser')} subject={subject}>
      <Suspense fallback={null}>
        <CloudBrowserView key={sessionId} sessionId={sessionId} />
      </Suspense>
    </InspectorTab>
  );
}

/**
 * The thread's Browser tab: the cloud run's streamed browser for a thread
 * away from this computer, the local pane otherwise.
 */
export function ThreadBrowserTab({ conversationId, cloudSession, subject }: { readonly conversationId: string | undefined; readonly cloudSession: string | null | undefined; readonly subject: string }): React.JSX.Element | null {
  if (cloudSession === null) return null;
  if (cloudSession !== undefined) return <CloudBrowserTab sessionId={cloudSession} subject={subject} />;
  if (conversationId === undefined || conversationId === '') return null;
  return <BrowserTab threadId={conversationId} subject={subject} />;
}
