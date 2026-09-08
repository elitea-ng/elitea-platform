/**
 * "Share by link" — the owner's dialog for publishing a conversation behind a
 * token, seeing the links already on it, and revoking them.
 *
 * # The one-shot token, and what the UI has to do about it
 *
 * The server stores only SHA-256 of the token, so the plaintext link exists in
 * exactly one response: the create call's. There is no way to show it again,
 * and no listing that could. The dialog is therefore built around that fact
 * rather than around it: the freshly created URL is copied to the clipboard
 * immediately AND left on screen with an explicit warning that it will not be
 * shown again, and the "active links" list below carries metadata and a Revoke
 * button with no Copy.
 *
 * The SPA this is ported from does have a Copy on every listed link, because
 * its server hands the token back on every read. That is the divergence, and
 * this is where a user meets it.
 */
import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useState } from 'react';

import { SHARED_CHAT_LINKS_QUERY_KEY, useCreateShareLinkMutation, useRevokeShareLinkMutation, useShareLinksQuery, type ShareLinkExpiry, type SharedChatLink } from '@/entities/conversation';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import { buildSharedConversationUrl, getConversationShareBasename } from '../../lib/shareUrl';

const EXPIRY_OPTIONS: readonly { readonly value: ShareLinkExpiry; readonly label: string }[] = [
  { value: '1h', label: t('features.chatConversationList.shareLink.expiry.1h', '1 hour') },
  { value: '1d', label: t('features.chatConversationList.shareLink.expiry.1d', '1 day') },
  { value: '7d', label: t('features.chatConversationList.shareLink.expiry.7d', '7 days') },
  { value: '30d', label: t('features.chatConversationList.shareLink.expiry.30d', '30 days') },
];

/**
 * There is no "Never" option, and its absence is the point: the server refuses
 * an expiry it does not recognise rather than defaulting, and `expires_at` is
 * NOT NULL in the table. A link with no end of life is unrepresentable, not
 * merely un-offered here.
 */
const MIN_PASSWORD_LENGTH = 8;

export interface ShareLinkDialogProps {
  readonly open: boolean;
  readonly projectId: string | number;
  readonly conversationId: string | number;
  readonly conversationName?: string;
  readonly onClose: () => void;
}

export function ShareLinkDialog(props: ShareLinkDialogProps): React.JSX.Element {
  const { open, projectId, conversationId, conversationName, onClose } = props;
  const queryClient = useQueryClient();

  const [expiry, setExpiry] = useState<ShareLinkExpiry>('7d');
  const [password, setPassword] = useState('');
  const [createdUrl, setCreatedUrl] = useState<string | null>(null);
  const [error, setError] = useState('');

  const links = useShareLinksQuery({ projectId, conversationId }, open);
  const create = useCreateShareLinkMutation();
  const revoke = useRevokeShareLinkMutation();

  const passwordValid = password.trim() === '' || password.trim().length >= MIN_PASSWORD_LENGTH;

  const invalidate = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: [SHARED_CHAT_LINKS_QUERY_KEY, String(projectId), String(conversationId)] });
  }, [conversationId, projectId, queryClient]);

  const handleCreate = useCallback(() => {
    setError('');
    create.mutate(
      {
        projectId,
        conversationId,
        expiry,
        ...(password.trim() !== '' ? { password: password.trim() } : {}),
      },
      {
        onSuccess: (link) => {
          // The BASENAME is part of the address: this router is mounted at
          // `/app/`, and a link that omitted it 404'd at the edge for its
          // recipient while looking correct to the person who created it.
          const url = buildSharedConversationUrl({
            origin: window.location.origin,
            basename: getConversationShareBasename(),
            token: link.token,
          });
          setCreatedUrl(url);
          setPassword('');
          // Best-effort: a clipboard write can be refused (no permission, no
          // secure context). The URL stays on screen either way, which is what
          // makes a refused write recoverable rather than a lost token.
          void navigator.clipboard?.writeText(url).catch(() => undefined);
          invalidate();
        },
        onError: () => setError(t('features.chatConversationList.shareLink.createFailed', 'Could not create the link. Please try again.')),
      },
    );
  }, [conversationId, create, expiry, invalidate, password, projectId]);

  const handleRevoke = useCallback(
    (linkId: number) => {
      revoke.mutate(
        { projectId, conversationId, linkId },
        {
          onSuccess: invalidate,
          onError: () => setError(t('features.chatConversationList.shareLink.revokeFailed', 'Could not revoke the link. Please try again.')),
        },
      );
    },
    [conversationId, invalidate, projectId, revoke],
  );

  const handleClose = useCallback(() => {
    setCreatedUrl(null);
    setPassword('');
    setError('');
    onClose();
  }, [onClose]);

  const content = (
    <Box sx={contentSx} data-testid="share-link-dialog">
      <Typography variant="body2" color="text.disabled">
        {t('features.chatConversationList.shareLink.warning', 'Anyone with the link can read this conversation, without signing in. Do not share transcripts that contain sensitive information.')}
      </Typography>

      <Select value={expiry} onChange={(event) => setExpiry(event.target.value)} inputProps={{ 'aria-label': t('features.chatConversationList.shareLink.expiryLabel', 'Link expires after') }} data-testid="share-link-expiry">
        {EXPIRY_OPTIONS.map((option) => (
          <MenuItem key={option.value} value={option.value}>
            {option.label}
          </MenuItem>
        ))}
      </Select>

      <TextField
        type="password"
        label={t('features.chatConversationList.shareLink.passwordLabel', 'Password (optional, at least 8 characters)')}
        value={password}
        onChange={(event) => setPassword(event.target.value)}
        error={!passwordValid}
        variant="standard"
        fullWidth
        slotProps={{ htmlInput: { 'data-testid': 'share-link-password' } }}
      />

      {error !== '' && (
        <Typography variant="body2" color="error.main" data-testid="share-link-error">
          {error}
        </Typography>
      )}

      {createdUrl !== null && (
        <Box sx={createdSx} data-testid="share-link-created">
          <Typography variant="body2" color="text.secondary" sx={urlSx} data-testid="share-link-created-url">
            {createdUrl}
          </Typography>
          <Typography variant="caption" color="text.disabled">
            {t('features.chatConversationList.shareLink.oneShot', 'Copied to your clipboard. This link is shown once and cannot be retrieved again — if you lose it, revoke it and create a new one.')}
          </Typography>
        </Box>
      )}

      <Typography variant="body2" color="text.secondary">
        {t('features.chatConversationList.shareLink.activeLinks', 'Links on this conversation')}
      </Typography>
      {links.data === undefined || links.data.length === 0 ? (
        <Typography variant="caption" color="text.disabled" data-testid="share-link-empty">
          {t('features.chatConversationList.shareLink.noLinks', 'No links yet.')}
        </Typography>
      ) : (
        links.data.map((link) => <ShareLinkRow key={link.id} link={link} onRevoke={handleRevoke} />)
      )}
    </Box>
  );

  return (
    <BaseModal
      open={open}
      onClose={handleClose}
      title={conversationName ?? t('features.chatConversationList.shareLink.title', 'Share by link')}
      content={content}
      actions={{
        node: (
          <>
            <BaseBtn variant="secondary" onClick={handleClose}>
              {t('features.chatConversationList.shareLink.close', 'Close')}
            </BaseBtn>
            <BaseBtn disabled={!passwordValid || create.isPending} onClick={handleCreate} data-testid="share-link-create">
              {t('features.chatConversationList.shareLink.create', 'Create link')}
            </BaseBtn>
          </>
        ),
      }}
    />
  );
}

function ShareLinkRow(props: { readonly link: SharedChatLink; readonly onRevoke: (linkId: number) => void }): React.JSX.Element {
  const { link, onRevoke } = props;
  return (
    <Box sx={rowSx} data-testid="share-link-row">
      <Box sx={rowMetaSx}>
        <Typography variant="caption" color="text.secondary">
          {link.active ? t('features.chatConversationList.shareLink.active', 'Active') : t('features.chatConversationList.shareLink.inactive', 'Revoked or expired')}
          {link.has_password ? t('features.chatConversationList.shareLink.protected', ' · Password protected') : ''}
        </Typography>
        <Typography variant="caption" color="text.disabled">
          {t('features.chatConversationList.shareLink.expires', 'Expires ')}
          {new Date(link.expires_at).toLocaleString()}
          {t('features.chatConversationList.shareLink.views', ' · Opened ')}
          {String(link.access_count)}
        </Typography>
      </Box>
      {link.active && (
        <BaseBtn variant="secondary" onClick={() => onRevoke(link.id)} data-testid="share-link-revoke">
          {t('features.chatConversationList.shareLink.revoke', 'Revoke')}
        </BaseBtn>
      )}
    </Box>
  );
}

const contentSx = { display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '24rem' } as const;
const createdSx = { display: 'flex', flexDirection: 'column', gap: '0.25rem' } as const;
/**
 * The link, and nothing else, on its own line.
 *
 * `userSelect: 'all'` makes one click select the WHOLE address and only the
 * address. The block it sits in also carries the "shown once" sentence, and
 * the two are adjacent text nodes with no separator between them: a reader
 * who drags across the box, and every reader of `share-link-created`'s
 * `textContent`, gets `…/shared/chat/<token>Copied to your clipboard…` —
 * the token with the next sentence's first word welded onto its end. That is
 * a broken link, and it is broken in the one place this app cannot show
 * again. `share-link-created-url` is therefore the element to read the URL
 * off, and the box around it is only the layout.
 */
const urlSx = { wordBreak: 'break-all', userSelect: 'all' } as const;
const rowSx = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '0.5rem' } as const;
const rowMetaSx = { display: 'flex', flexDirection: 'column' } as const;
