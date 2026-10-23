import { WorkspaceTitle } from '@/features/workspaces/WorkspaceTitle';

/**
 * Header options every signed-in stack shares (the outer stack and each tab's stack):
 * the screen title over the ACTIVE workspace's host, so the host is always on screen.
 */
export const workspaceHeaderOptions = {
  headerTitle: (props: { children: string }) => <WorkspaceTitle title={props.children} />,
} as const;
