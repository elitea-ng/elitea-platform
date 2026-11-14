import { useApplicationBulkActions } from '../model/useApplicationBulkActions';
import { useWorkshopListData } from '../model/useWorkshopListData';

import { statusBadgeLabel, workshopFacets } from './workshopFacets';

/**
 * What the Workshop's agents and pipelines lists take from this slice
 * (ADR-0051 M1), as one handle so the slice's public API stays curated:
 *  - `useListData`: the list's rows, narrowed and sorted, with the facet counts;
 *  - `facets`: the status and origin filters from the list's server counts;
 *  - `statusBadge`: a row's status as the word its badge shows;
 *  - `useBulkActions`: Add tag, Export and Delete over the selected rows.
 */
export const workshopList = {
  useListData: useWorkshopListData,
  facets: workshopFacets,
  statusBadge: statusBadgeLabel,
  useBulkActions: useApplicationBulkActions,
} as const;
