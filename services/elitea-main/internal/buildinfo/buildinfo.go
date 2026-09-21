// Package buildinfo carries this binary's own build identity — the one piece
// of "which version is this" data a running elitea-main process can answer
// honestly without reaching into another process or another repository.
//
// It exists for issue #892. `GET /admin/system_info/prompt_lib`
// (internal/api/v2/admin/handler.go) used to answer 501 unconditionally
// because this service had no build-version plumbing at all: the
// Containerfile declared `ARG VERSION=dev` and never used it, so there was
// nothing but "(devel)" from debug.ReadBuildInfo to report, and reporting
// that to an operator who asks which build is deployed would be the same
// failure in new clothes.
//
// Version is a package-level var, set at build time by `-ldflags -X` (see
// services/elitea-main/Containerfile), not read from debug.BuildInfo: the
// build copies services/elitea-main/ without a .git directory, so
// runtime/debug never has a module version or a revision to report here,
// with or without this package.
package buildinfo

// Version is this elitea-main binary's own release version. The Containerfile
// passes it via `-ldflags -X .../internal/buildinfo.Version=${VERSION}`, and
// VERSION is a real released version in CI (publish.yml sets it from the
// release step) and the literal string "dev" for every other build — a local
// `go build` outside the Containerfile included, which is the one case "dev"
// is an honest answer.
var Version = "dev"
