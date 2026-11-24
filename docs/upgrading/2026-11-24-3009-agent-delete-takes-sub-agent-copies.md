# Deleting an agent deletes its sub-agent copies (#3009)

**What changed.** An agent hired from the Marketplace, or published with
sub-agents, holds a private copy of each sub-agent: an application whose
only version has status `embedded`. The Workshop lists those copies as
drafts, but `DELETE /api/v2/elitea_core/application/prompt_lib/{project}/{id}`
refused every one of them with `400` ("Unpublish first"), and deleting the
parent agent left them behind. So a team project filled up with agents that
could not be deleted.

- Deleting an agent now deletes, in the same transaction, the embedded copies
  its versions reference (down to six levels), and the sub-agent references
  (`entity_tool_mapping` rows and `elitea_tools` rows of type `application`)
  of its versions. A copy that a version of another agent still references is
  kept.
- An embedded copy that no other agent references (an orphan left by an
  earlier release) can now be deleted on its own.
- An embedded copy that another agent still references is refused with `400`
  and the sentence "This agent is the sub-agent copy of another agent. Delete
  or unpublish that agent, and this copy goes with it."
- A published agent is refused as before ("Unpublish first. Cannot delete
  application with published versions."), and the Workshop delete dialog and
  the bulk delete now show the server's sentence instead of a generic one.

**Who is affected.** Projects with hired or published agents. Orphaned copies
from earlier releases become deletable from the Workshop; nothing is deleted
without a user's delete.

**Rollback.** The previous release refuses embedded copies again. Nothing is
migrated.
