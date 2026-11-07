/**
 * The chat's Browser tab (ADR-0048 decision 7, #1598): the streamed view of
 * the cloud browser the newest run used, in the shell's right panel (the web
 * side panel, or the desktop inspector for a cloud thread). Offered only
 * once an answer used the browser tools, and not for a Local work thread
 * that runs on the desktop (its pane is the desktop's own webview).
 *
 * `useCloudBrowserSession` reads ChatBox's history mirror and keeps only the
 * session name, so a streamed chunk re-renders nothing here. The tab is not
 * brought forward by itself (on a narrow window the panel is a drawer over
 * the chat); its chip appears. The pane's code loads when the tab is shown.
 */
import { lazy, Suspense, useCallback, useRef, useState, type Dispatch, type SetStateAction } from 'react';

import { browserViewAvailable } from '@/shared/api/browserView/client';
import { t } from '@/shared/i18n';
import { SidePanelTab } from '@/shared/ui/side-panel';

import { cloudBrowserSessionOf } from './cloudBrowserSession';

const CloudBrowserView = lazy(() => import('@/shared/ui/CloudBrowserView').then((module) => ({ default: module.CloudBrowserView })));

type HistorySink = Dispatch<SetStateAction<readonly unknown[]>>;

/** The session to show, and the sink to hand ChatBox as `setChatHistory`. */
export function useCloudBrowserSession(enabled: boolean): { readonly sessionId: string | undefined; readonly onHistory: HistorySink } {
  const [sessionId, setSessionId] = useState<string | undefined>(undefined);
  const last = useRef<readonly unknown[]>([]);
  const onHistory = useCallback<HistorySink>((next) => {
    const messages = typeof next === 'function' ? next(last.current) : next;
    last.current = messages;
    setSessionId(cloudBrowserSessionOf(messages));
  }, []);
  return { sessionId: enabled && browserViewAvailable() ? sessionId : undefined, onHistory };
}

export interface ChatCloudBrowserTabProps {
  readonly sessionId: string | undefined;
  readonly conversationName: string;
}

export function ChatCloudBrowserTab({ sessionId, conversationName }: ChatCloudBrowserTabProps): React.JSX.Element | null {
  if (sessionId === undefined) return null;
  return (
    <SidePanelTab id="cloud-browser" title={t('cloudBrowser.tab', 'Browser')} subject={conversationName}>
      <Suspense fallback={null}>
        <CloudBrowserView key={sessionId} sessionId={sessionId} />
      </Suspense>
    </SidePanelTab>
  );
}
