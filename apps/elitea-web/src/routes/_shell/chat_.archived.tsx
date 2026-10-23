/**
 * `/chat/archived` -> `ArchivedChatsPage`: the conversations the caller
 * archived (client contract 1.10). New in this app (the baseline has no
 * archive). `chat_` keeps the route OUT of `chat.tsx`'s layout, which renders
 * the whole chat surface around its `<Outlet/>`; the static segment wins over
 * `chat.$conversationId.tsx`'s param. Gated like `/chat` itself.
 */
import { createFileRoute } from '@tanstack/react-router';

import ArchivedChatsPage from '@/pages/chat-archived/ArchivedChatsPage';

import { requireChatPermission } from '../-guards/requirePermission';
import { RouteError, RoutePending } from '../-ui/RouteStatus';

export const Route = createFileRoute('/_shell/chat_/archived')({
  beforeLoad: requireChatPermission,
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: ArchivedChatsRoute,
});

function ArchivedChatsRoute() {
  return <ArchivedChatsPage />;
}
