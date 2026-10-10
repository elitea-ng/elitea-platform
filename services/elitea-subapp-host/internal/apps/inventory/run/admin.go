package run

// The inventory_admin family (issue #1244, ADR-0031 decision 7, phase C0):
// the platform deleting what a deleted Inventory toolkit or project left in
// the engine's graph store.
//
//	delete_graph           one toolkit's graph (project from the verified
//	                       identity, toolkit from application_id)
//	delete_project_graphs  every graph of the verified identity's project
//
// The family is in the admission table and NOT in the descriptor, so the
// facade, which admits a tool only if the descriptor advertises it, never
// offers either to a user or an agent. They are for the platform's own
// calls:
//
//   - elitea-main, when an Inventory toolkit (an application) is deleted,
//     invokes delete_graph with the project signed in the identity and the
//     toolkit in application_id;
//   - elitea-main's project deprovisioning invokes delete_project_graphs with
//     the project signed in the identity and NO user. A call from a user
//     session, and a call whose identity is not signature-verified, are
//     refused here before the engine is reached.
//
// The tools go to the engine unchanged (the engine keys everything by the
// project this runner stamps from the verified identity, as for every
// Inventory tool). The host adds no deletion of its own: the graph store is
// the engine's.

import (
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const (
	// AdminFamily is the family's name, and its only alias.
	AdminFamily = "inventory_admin"
	// DeleteGraphTool deletes one toolkit's graph in the engine.
	DeleteGraphTool = "delete_graph"
	// DeleteProjectGraphsTool deletes every graph of the identity's project.
	DeleteProjectGraphsTool = "delete_project_graphs"
)

// CheckAdmin refuses an inventory_admin call that is not the platform's.
//
//   - Both tools need a project in the VERIFIED identity (the identity gate
//     empties an identity whose signature does not verify). On the engine
//     runner Invoke has already refused a call without one; this holds for
//     any runner.
//   - delete_project_graphs needs NO user in it: a project-wide delete is
//     project deprovisioning, which signs the project alone. A user session
//     always carries its user id and is refused.
//   - delete_graph needs the toolkit it deletes.
func CheckAdmin(tool string, identity spi.Identity, toolkit any) error {
	if identity.ProjectID == "" {
		return spi.Failf(spi.KindValue,
			"%s needs a verified platform identity that names the project; this call carries none", tool)
	}
	switch tool {
	case DeleteProjectGraphsTool:
		if identity.UserID != "" {
			return spi.Failf(spi.KindValue,
				"%s is a platform operation (project deletion) and cannot be run from a user session", tool)
		}
	case DeleteGraphTool:
		if !Truthy(toolkit) {
			return spi.Failf(spi.KindValue,
				"%s needs the Inventory toolkit (application_id) whose graph it deletes", tool)
		}
	}
	return nil
}
