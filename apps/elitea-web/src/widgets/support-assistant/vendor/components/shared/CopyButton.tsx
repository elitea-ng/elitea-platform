import React, { memo, useCallback, useEffect, useRef, useState } from 'react';

import { CheckIcon, CopyIcon } from '../icons';
import Tooltip from './Tooltip';

import { t } from '@/shared/i18n';

const CopyButton: React.FC<{ text: string }> = memo(props => {
  const { text } = props;

  const [copied, setCopied] = useState(false);
  // The tick's reset timer must die with the button: a timer that outlives
  // the unmount calls setState on a component React has released, which is
  // an uncaught "window is not defined" when a unit shard has already torn
  // the DOM down (unit shard 1 on df2d5c81).
  const resetTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (resetTimer.current !== null) clearTimeout(resetTimer.current);
    },
    [],
  );

  const handleCopy = useCallback(() => {
    // A clipboard write can reject (permission, insecure context) and the
    // reference already ignores that — the tick still flashes, and the user
    // finds out by pasting. Failing loudly here would be worse than the tick.
    void navigator.clipboard.writeText(text).catch(() => undefined);
    setCopied(true);
    if (resetTimer.current !== null) clearTimeout(resetTimer.current);
    resetTimer.current = setTimeout(() => setCopied(false), 2000);
  }, [text]);

  return (
    <Tooltip content={t('widgets.supportAssistant.copy', 'Copy to clipboard')}>
      <button
        className="elitea-assistant-header-action"
        onClick={handleCopy}
        aria-label={t('widgets.supportAssistant.copy', 'Copy to clipboard')}
        type="button"
      >
        {copied ? <CheckIcon /> : <CopyIcon />}
      </button>
    </Tooltip>
  );
});

CopyButton.displayName = 'CopyButton';

export default CopyButton;
