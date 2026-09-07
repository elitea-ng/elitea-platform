import React, { memo } from 'react';

import ChatHeader from './ChatHeader';
import MessageInput from './MessageInput';
import MessageList from './MessageList';
import { useChat } from '../../lib/hooks';
import type { TConversationListItem, TRawConversation } from '../../lib/types';

import { t } from '@/shared/i18n';

type TChatWindowProps = {
  avatar: string;
  title: string;
  placeholder: string;
  welcomeMessage: string;
  supportProjectId: number | null;
  initialHistory: TConversationListItem[];
  lastConversation: TRawConversation | null;
  isInitLoading: boolean;
  isOpen: boolean;
  onClose: () => void;
  onExpand?: (() => void) | undefined;
  expanded?: boolean | undefined;
};

const ChatWindow: React.FC<TChatWindowProps> = memo(props => {
  const {
    avatar,
    title,
    placeholder,
    welcomeMessage,
    supportProjectId,
    initialHistory,
    lastConversation,
    isInitLoading,
    isOpen,
    onClose,
    onExpand,
    expanded,
  } = props;

  const {
    messages,
    inputText,
    setInputText,
    history,
    currentConversationId,
    isLoading,
    isStreaming,
    handleNewChat,
    handleSelectConversation,
    handleSend,
    handleAnimationComplete,
  } = useChat({
    welcomeMessage,
    supportProjectId,
    initialHistory,
    initialConversation: lastConversation,
    isInitLoading,
  });

  if (!isOpen) return null;

  const window = (
    <div className={`elitea-assistant-window${expanded ? ' elitea-assistant-window--expanded' : ''}`}>
      <ChatHeader
        title={title}
        expanded={expanded}
        history={history}
        currentConversationId={currentConversationId}
        disabled={isLoading}
        onClose={onClose}
        onExpand={onExpand}
        onNewChat={handleNewChat}
        onSelectConversation={conversationId => void handleSelectConversation(conversationId)}
      />
      <MessageList
        avatar={avatar}
        messages={messages}
        isLoading={isLoading}
        onAnimationComplete={handleAnimationComplete}
      />
      <MessageInput
        placeholder={placeholder}
        text={inputText}
        onTextChange={setInputText}
        onSend={(text, file) => void handleSend(text, file)}
        disabled={isLoading || isStreaming}
      />
    </div>
  );

  return (
    <>
      {expanded && (
        // A DISMISS CONTROL, not decoration: clicking the scrim collapses
        // fullscreen. The reference renders it as a bare `div`, so a keyboard
        // user had no way to leave fullscreen except the header button. Given a
        // role and Escape, it is reachable the way a modal backdrop should be.
        <button
          type="button"
          className="elitea-assistant-overlay"
          aria-label={t('widgets.supportAssistant.collapseAssistant', 'Collapse the assistant')}
          onClick={onExpand}
        />
      )}
      {window}
    </>
  );
});

ChatWindow.displayName = 'ChatWindow';

export default ChatWindow;
