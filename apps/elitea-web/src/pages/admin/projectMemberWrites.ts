/**
 * The project member writes of Admin › Projects, each through the read-only
 * guard of the projects item (ADR-0059 decision 6; R3.4b, issue 2613): at
 * `read` a write sends nothing, whatever control reached it.
 */
import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import { useAddProjectMember, useRemoveProjectMember, useUpdateProjectMemberRole } from './api/adminProjectsApi';

export function useProjectMemberWrites() {
  const access = useAdminWritable(ADMIN_ITEM.projects);
  return {
    addMember: access.guard(useAddProjectMember()),
    updateRole: access.guard(useUpdateProjectMemberRole()),
    removeMember: access.guard(useRemoveProjectMember()),
  };
}
