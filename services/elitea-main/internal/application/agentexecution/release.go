package agentexecution

// ReleaseRun names the agent release a run executes (ADR-0040 decision 2,
// issue #1492). The start resolver sets it on the target when the run is a
// release run: a run that names a release id, or a run of a published
// version whose application has a published release. The resolver has then
// already put the release's definition into the target and checked its
// toolkit pins (a drifted pin refuses the start with
// agentrelease.DriftError).
//
// The admission records ID on the run (agent_execution_jobs.release_id). A
// governed admission binds the agent binding of ApplicationVersionID, the
// version row the release was published as, and compares it with
// DefinitionDigest, the digest of the definition the run executes, instead
// of the version the participant names and its row as it stands now. So a
// rollback to an earlier release runs under that release's binding.
type ReleaseRun struct {
	ID                   int64
	ApplicationVersionID int64
	DefinitionDigest     [32]byte
}

// bindingVersionID is the version whose agent binding a governed run binds:
// the release's version row for a release run, else versionID.
func bindingVersionID(release *ReleaseRun, versionID int64) int64 {
	if release != nil && release.ApplicationVersionID > 0 {
		return release.ApplicationVersionID
	}
	return versionID
}

// releaseID is the id of the run's release, zero for none.
func releaseID(release *ReleaseRun) int64 {
	if release == nil {
		return 0
	}
	return release.ID
}

// Clone copies the run's release.
func (run *ReleaseRun) Clone() *ReleaseRun {
	if run == nil {
		return nil
	}
	clone := *run
	return &clone
}
