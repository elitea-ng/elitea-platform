/**
 * Does this project have a saved Project Context?
 *
 * This is the test that chooses between the two screens the reference splits
 * this tab into (`ProjectContextContent.jsx:56-77`): a project with content
 * gets the editor, a project without gets the centred invitation.
 *
 * IT READS THE SERVER'S CONTENT, NOT THE EDITOR'S BUFFER. Once "Create" is
 * pressed the reader is typing into an editor whose local `content` is still
 * `''`; keying the branch off that state would snap them back to the
 * invitation on the first keystroke that emptied the box again, losing what
 * they had typed. The editor's own `isEditing` flag is the other half of the
 * condition, and it lives in the component.
 *
 * Whitespace does not count as content — a context of three spaces is not a
 * context, and the reference agrees (`Boolean(serverData?.content?.trim())`).
 */
export function hasSavedProjectContext(serverData: unknown): boolean {
  if (typeof serverData !== 'object' || serverData === null) return false;
  const content = (serverData as { content?: unknown }).content;
  return typeof content === 'string' && content.trim().length > 0;
}
