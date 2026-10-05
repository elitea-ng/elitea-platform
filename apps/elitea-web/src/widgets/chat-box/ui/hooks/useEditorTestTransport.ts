/** Test observer attachment and strict transport selection create no work. */
import { useCallback, useEffect } from 'react';
import type { UseChatStreamTransportResult } from '@/features/chat-messages';
import { t } from '@/shared/i18n';
import type { ChatBoxProps } from '../ChatBox.types';
import type { StreamStartOutcome } from './useChatBoxHandlers.helpers';
type EditorTest = NonNullable<ChatBoxProps['extensions']>['editorTest'];
export function useEditorTestTransport(editorTest: EditorTest, transport: UseChatStreamTransportResult): (outcome: StreamStartOutcome) => StreamStartOutcome {
 const {attachExistingRun,close}=transport;const restoredRun=editorTest?.restoredRun;
 useEffect(()=>{if(!restoredRun)return;attachExistingRun(restoredRun);return close;},[restoredRun,attachExistingRun,close]);
 return useCallback((outcome:StreamStartOutcome)=>editorTest && !outcome.started && outcome.reason==='no-transport'
  ? {started:false,reason:'rejected',message:t('pages.pipelines.testChat.transportUnavailable','Durable Test execution is unavailable. Retry after Main is updated.')}
  : outcome,[editorTest]);
}
export function stopChatGeneration(stopStream:()=>void,stopSocket:()=>void,editorTest:EditorTest):void {
 stopStream();if(!editorTest)stopSocket();
}
