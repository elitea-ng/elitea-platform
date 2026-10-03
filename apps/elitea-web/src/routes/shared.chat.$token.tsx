/**
 * `/shared/chat/:token` — the page a share link opens.
 *
 * A TOP-LEVEL route, sibling of `auth-callback`, NOT nested under `_shell`.
 * That is the whole placement decision: `_shell` is the signed-in application
 * chrome — sidebar, project selector, permission-gated navigation — and every
 * visitor this page exists for has none of it. Nesting it there would render
 * an app frame around a page whose reader has no project and no session, and
 * would run the shell's own data loading against an API that will refuse it.
 *
 * Eager, not lazy, for the same reason `auth-callback` is: this route is
 * reached by a cold navigation from an external link, so there is no already-
 * loaded application to code-split away from.
 *
 * All four states the server can put this page in are rendered here, and the
 * two refusals are ONE state on purpose. The server answers an unknown, a
 * revoked and an expired token identically so that a guesser cannot tell which
 * tokens were ever real; this page must not undo that by inventing a "this link
 * expired" message it has no way to earn. (The SPA this is ported from branches
 * on a 410 to say exactly that — the divergence is deliberate.)
 */
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import { createFileRoute } from '@tanstack/react-router';
import { useCallback, useEffect, useState } from 'react';

import { fetchSharedConversation, unlockSharedConversation, type SharedChatMessage, type SharedChatViewResult } from '@/shared/api/sharedChatView';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { Markdown } from '@/shared/ui/Markdown';

export const Route = createFileRoute('/shared/chat/$token')({
  component: SharedConversationPage,
});

type PageState = SharedChatViewResult | { readonly status: 'loading' };

function SharedConversationPage(): React.JSX.Element {
  const { token } = Route.useParams();
  const [state, setState] = useState<PageState>({ status: 'loading' });
  const [password, setPassword] = useState('');
  const [unlockError, setUnlockError] = useState('');
  const [unlocking, setUnlocking] = useState(false);

  const load = useCallback(() => {
    let cancelled = false;
    setState({ status: 'loading' });
    void fetchSharedConversation(token).then((result) => {
      if (!cancelled) setState(result);
    });
    return () => {
      cancelled = true;
    };
  }, [token]);

  useEffect(() => load(), [load]);

  const handleUnlock = useCallback(() => {
    setUnlockError('');
    setUnlocking(true);
    void unlockSharedConversation(token, password).then((outcome) => {
      setUnlocking(false);
      if (outcome === 'ok') {
        setPassword('');
        load();
        return;
      }
      // 'rejected' covers BOTH a wrong password and a link that does not
      // exist — the server refuses to distinguish them, and one message is
      // how this page keeps that promise.
      setUnlockError(
        outcome === 'rejected'
          ? t('sharedConversation.unlock.rejected', 'That password did not work.')
          : t('sharedConversation.unlock.error', 'Something went wrong. Please try again.'),
      );
    });
  }, [load, password, token]);

  if (state.status === 'loading') {
    return (
      <Box sx={centeredSx} data-testid="shared-conversation-loading">
        <CircularProgress size={32} />
      </Box>
    );
  }

  if (state.status === 'unavailable' || state.status === 'error') {
    return (
      <Box sx={centeredSx} data-testid="shared-conversation-unavailable">
        <Typography variant="headingMedium" component="h1" color="text.secondary">
          {t('sharedConversation.unavailable.title', 'Link not available')}
        </Typography>
        <Typography variant="bodyMedium" component="p" color="text.disabled" sx={subtitleSx}>
          {t('sharedConversation.unavailable.body', 'This shared conversation link is invalid, has been revoked, or has expired.')}
        </Typography>
      </Box>
    );
  }

  if (state.status === 'locked') {
    return (
      <Box sx={centeredSx} data-testid="shared-conversation-locked">
        <Box sx={passwordCardSx}>
          <Typography variant="headingMedium" component="h1" color="text.secondary">
            {t('sharedConversation.locked.title', 'Password required')}
          </Typography>
          <Typography variant="bodyMedium" component="p" color="text.disabled" sx={subtitleSx}>
            {t('sharedConversation.locked.body', 'This conversation is password protected.')}
          </Typography>
          <input
            aria-label={t('sharedConversation.locked.passwordLabel', 'Password')}
            type="password"
            value={password}
            data-testid="shared-conversation-password"
            onChange={(event) => setPassword(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === 'Enter' && password.trim() !== '') handleUnlock();
            }}
          />
          {unlockError !== '' && (
            <Typography variant="bodyMedium" component="p" color="error.main" data-testid="shared-conversation-unlock-error">
              {unlockError}
            </Typography>
          )}
          <BaseBtn disabled={unlocking || password.trim() === ''} onClick={handleUnlock}>
            {t('sharedConversation.locked.unlock', 'Unlock')}
          </BaseBtn>
        </Box>
      </Box>
    );
  }

  const { conversation } = state;
  return (
    <Box sx={pageSx} data-testid="shared-conversation">
      <Box sx={headerSx}>
        <Typography variant="headingLarge" component="h1" color="text.secondary">
          {conversation.conversation_name}
        </Typography>
        <Typography variant="bodySmall" color="text.disabled">
          {t('sharedConversation.header.readOnly', 'Shared conversation · Read only')}
        </Typography>
      </Box>
      <Box sx={messagesSx}>
        {conversation.messages.length === 0 && (
          <Typography variant="bodyMedium" component="p" color="text.disabled" data-testid="shared-conversation-empty">
            {t('sharedConversation.empty', 'No messages to display for this link.')}
          </Typography>
        )}
        {conversation.messages.map((message) => (
          <SharedMessageGroup key={message.id} message={message} />
        ))}
      </Box>
    </Box>
  );
}

function SharedMessageGroup(props: { readonly message: SharedChatMessage }): React.JSX.Element {
  const { message } = props;
  return (
    <Box sx={groupSx} data-testid="shared-conversation-message">
      <Box sx={authorRowSx}>
        <Typography variant="bodyMedium" component="p" color="text.secondary">
          {message.author_name ?? (message.author_type === 'user' ? t('sharedConversation.author.user', 'User') : t('sharedConversation.author.assistant', 'Assistant'))}
        </Typography>
        <Typography variant="bodySmall" color="text.disabled">
          {new Date(message.created_at).toLocaleString()}
        </Typography>
      </Box>
      <Box sx={bodySx}>
        {message.is_error && (
          <Typography variant="bodyMedium" component="p" color="error.main">
            {/*
             * The FLAG only. The server sends no error text, because an
             * upstream error routinely quotes the offending fragment of the
             * request back — which on this page would be an anonymous
             * disclosure of the prompt.
             */}
            {t('sharedConversation.message.error', 'This message failed.')}
          </Typography>
        )}
        {!message.is_error &&
          message.items.map((item, index) => {
            if (item.attachment !== undefined) {
              // Named, never linked: no anonymous byte route exists.
              return (
                <Typography key={index} variant="bodyMedium" component="p" color="text.disabled" data-testid="shared-conversation-attachment">
                  {t('sharedConversation.message.attachment', 'Attachment: ')}
                  {item.attachment.name}
                </Typography>
              );
            }
            // `renderHtml={false}`. The default renders literal HTML written
            // inside the message, and every byte of this content is
            // user-authored text on a page any anonymous visitor can load with
            // no session. `Token` sanitizes, but the safest markup budget for
            // an unauthenticated surface is "no author-supplied markup at
            // all" — markdown formatting still renders, raw HTML does not.
            return (
              <Markdown key={index} renderHtml={false}>
                {item.content ?? ''}
              </Markdown>
            );
          })}
      </Box>
    </Box>
  );
}

const centeredSx = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  justifyContent: 'center',
  minHeight: '100vh',
  padding: '2rem',
  textAlign: 'center',
} as const;

const subtitleSx = { marginTop: '0.5rem', maxWidth: '28rem' } as const;

const passwordCardSx = {
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
  padding: '2rem',
  borderRadius: 'var(--el-shape-radiusMd, 8px)',
  width: '100%',
  maxWidth: '24rem',
} as const;

const pageSx = { display: 'flex', flexDirection: 'column', minHeight: '100vh' } as const;

const headerSx = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  padding: '1.5rem 2rem',
} as const;

const messagesSx = {
  display: 'flex',
  flexDirection: 'column',
  padding: '0.75rem 2rem',
  maxWidth: '52rem',
  width: '100%',
  margin: '0 auto',
} as const;

const groupSx = { display: 'flex', flexDirection: 'column', gap: '0.5rem', padding: '0.75rem 0' } as const;

const authorRowSx = { display: 'flex', flexDirection: 'row', alignItems: 'center', gap: '0.5rem' } as const;

const bodySx = { borderRadius: 'var(--el-shape-radiusSm, 4px)', padding: '0.75rem 1rem', width: '100%', boxSizing: 'border-box' } as const;
