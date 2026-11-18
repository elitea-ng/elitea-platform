import type { ReactNode } from 'react';
import { useCallback } from 'react';

import Typography from '@mui/material/Typography';

import { useNavigate } from '@tanstack/react-router';

import { WIKI_TOOLKIT_TYPES, useApplicationToolkits } from '@/features/apps';
import { ToolkitsList, type ToolkitListItem } from '@/features/toolkits';
import { t } from '@/shared/i18n';
import { EntityCard, entityTypeIcon } from '@/shared/ui/EntityCardList';

function noop(): void {
  // The list is one page: `useApplicationToolkits` asks for every toolkit at once.
}

/**
 * The Applications tab: the project's Wiki and Inventory applications. An
 * application is a toolkit instance of an application type, so each card opens
 * the native screen of that type (`/deepwiki/:id`, `/inventory/:id`) (#2113).
 */
export function ApplicationsTab(): ReactNode {
  const navigate = useNavigate();
  const { applications, isLoading, isError } = useApplicationToolkits();

  const open = useCallback(
    (item: ToolkitListItem) => {
      if (WIKI_TOOLKIT_TYPES.includes(item.type)) {
        void navigate({ to: '/deepwiki/$toolkitId', params: { toolkitId: item.id } });
      } else {
        void navigate({ to: '/inventory/$toolkitId', params: { toolkitId: item.id } });
      }
    },
    [navigate],
  );

  return (
    <div data-testid="apps-applications-tab-panel">
      <ToolkitsList
        data={applications}
        listState={{ isLoading, isError, hasMore: false, onLoadMore: noop, totalCount: applications.length }}
        emptyListPlaceHolder={
          <Typography
            variant="bodyMedium"
            component="p"
            sx={{ padding: '2rem' }}
          >
            {t('pages.apps.applications.empty', 'No applications yet. Configure one from the App Catalog.')}
          </Typography>
        }
        renderCard={(item) => (
          <EntityCard
            key={item.id}
            data-testid="application-card"
            item={{
              id: item.id,
              name: item.name,
              icon: entityTypeIcon('toolkit'),
              tags: [{ id: item.type, name: item.type }],
              typeLabel: item.type,
              onClick: () => open(item),
            }}
          />
        )}
      />
    </div>
  );
}
