import type { ReactNode } from 'react';
import { memo } from 'react';

import { t } from '@/shared/i18n';
import { visuallyHidden } from '@/shared/lib/visuallyHidden';

import { useTypewriter } from '../../lib/hooks';
import type { TMessage } from '../../lib/types';
import MarkdownContent from './MarkdownContent';

type TAnimatedMessageProps = {
  message: TMessage;
  onComplete: () => void;
};

const AnimatedMessage = memo(({ message, onComplete }: TAnimatedMessageProps): ReactNode => {
  const { displayedText, skipAnimation } = useTypewriter(message.content, !!message.isAnimating, onComplete);

  const content = (
    <MarkdownContent
      isAnimating
      content={displayedText}
    />
  );

  /*
   * WHILE IT IS TYPING, the answer is a control: clicking it skips to the full
   * text. The reference makes that a bare `div` with an `onClick`, so the only
   * way to skip was a mouse. A real `button` gives it keyboard activation and a
   * name for nothing.
   *
   * ONCE THE ANIMATION IS DONE it is not a control any more, so it renders as
   * plain content — an inert focus stop on every finished answer would put a tab
   * stop between the user and the message box for each message in the history.
   */
  if (!message.isAnimating) return <div>{content}</div>;

  return (
    <button
      type="button"
      className="elitea-assistant-skip-animation"
      onClick={skipAnimation}
    >
      {/* Hidden text, not an aria-label: the visible answer must stay in the name (WCAG 2.5.3). */}
      <span style={visuallyHidden}>{t('widgets.supportAssistant.showFullAnswer', 'Show the full answer')}</span>
      {content}
    </button>
  );
});

AnimatedMessage.displayName = 'AnimatedMessage';

export default AnimatedMessage;
