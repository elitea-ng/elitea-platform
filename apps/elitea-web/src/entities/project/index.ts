/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 */
export type { Project, ProjectContext } from './model/types';
export type { ProjectContextEntry, ProjectInfoEntry, UploadedIcon } from './model/projectContextTypes';
export { isPersonalProjectName, isPublicProject, isSuspendedProject, sortProjectsByName } from './model/selectors';

/* ── API hooks ─────────────────────────────────────────────────────────── */

/* Handwritten: project info, project icons, context draft */
export {
  useProjectInfoQuery,
  useUpdateProjectInfoMutation,
} from './api/projectContextApi';
export {
  useProjectIconsQuery,
  useUploadProjectIconMutation,
  useDeleteProjectIconMutation,
} from './api/projectContextApi';
export {
  useGenerateProjectContextDraftMutation,
} from './api/projectContextApi';

/* Note: project_context CRUD uses generated names from applications.ts:
   useGetProjectContext, useUpdateProjectContext  */
