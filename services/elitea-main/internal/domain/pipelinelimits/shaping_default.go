//go:build !graph_extensions_rehearsal

package pipelinelimits

// shapingNodesAdmitted is false in production builds: the Worker compiler
// admits SplitOut and Aggregate only with its graph-extensions-rehearsal
// feature (compiler.rs SHAPING_INTEGRATION_READY).
const shapingNodesAdmitted = false
