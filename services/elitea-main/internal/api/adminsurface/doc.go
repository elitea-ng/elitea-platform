// Package adminsurface finds the administration surface of elitea-main by
// driving the composed router (ADR-0059 decision 1, story R0.1b).
//
// # The definition
//
// The administration surface is every route whose decision resolves the
// `administration` mode, wherever it is mounted and whether the check is a
// route gate or in a handler.
//
// "Wherever it is mounted" matters because the surface is wider than the
// `/admin` block of the router: budgets live under `/elitea_core`, the
// administration secrets under `/secrets`, project create and delete under
// `/projects` and the tracing status under `/tracing`. "Whether the check is a
// route gate or in a handler" matters because part of the surface is decided
// inside a handler: a Configuration section names its permission in its
// schema, and the handler resolves it only after it has read the section. A
// test that reads one file or one path prefix misses both.
//
// # How it is found
//
// Drive, do not parse. Drive sends a request to every mounted (method,
// pattern), authenticated as a synthetic principal, and answers every
// permission resolution through the recording resolver of this package. The
// resolver records, for each resolution, the mode, the project and the
// function that asked.
//
// A resolver only answers a permission SET; it never learns which name the
// caller then looks for. Drive learns the names by probing each
// administration-mode resolution in turn: the other resolutions of the request
// answer every registered name, and the probed one answers nothing, every
// registered name, or one name at a time. A name the check accepts is one whose
// answer gives the same outcome (status, body and later resolutions) as
// granting every name. The two passes of the story are the two ends of that
// probe: the pass that grants nothing records what the route gate asks and
// stops at 403; the pass that grants everything reaches the handler, so the
// in-handler checks are recorded too.
//
// A check whose outcome no registered name changes is NOT treated as part of
// the registry. It either names a permission outside the registry, or its
// effect cannot be seen in the composed test router. The registry test fails
// on it unless it is listed, with a reason, as an observed-but-invisible
// check.
//
// # Absence is not an answer
//
// Many handlers have no store in the composed test router and answer 503
// before they resolve anything. Drive records a route that made no
// permission resolution at all as Unreached, with the status it answered, and
// so too a route whose path names the administration plane (/api/v2/admin, an
// `administration` segment) but that resolved other modes only. A test must
// fail on such a route unless it is declared with a reason, so that a route
// the helper could not reach, or a wrong gate, never reads as a route outside
// the surface. A request that times out, or a route that answers the same
// request two ways, is reported too: neither answer is evidence.
//
// # Checks the default request does not reach
//
// A Variant reaches a check the default request (a bearer token, the window
// closed, nothing granted in the other modes) never meets. ComposedRouter
// drives these (SessionAndWindowVariants, ADR-0059 #2925):
//
//   - the maintenance bypass, in the gate and in the platform_settings read,
//     with a window open (Variant.Maintenance). No database holds the window:
//     ComposedRouter reads it through api.RouterConfig.MaintenanceState, which
//     opens it for the requests of such a variant only;
//   - the personal token mint, whose administration-mode check runs for a
//     native device session only (Variant.Authenticate, NativeToken);
//   - the Marketplace moderation check, behind the project gate of the
//     agent list (Variant.OtherModes) and after the body is read.
//
// # What Drive cannot see
//
//   - A check whose answer feeds a query: the Marketplace reads hand the
//     moderation answer to the listing query, which the unconnected pool
//     fails whatever the grant. The listing update is driven instead; it
//     asks the same function.
//   - A route cmd/elitea-main adds through a RouterConfig field that the
//     composed router leaves unset. The admin SPA shell (adminui) is one; it
//     is presentation only and makes no decision. The cmd/elitea-main test
//     TestTheSurfaceRouterSetsEveryRouteGroupMainSets lists every handler or
//     store field main.go sets and ComposedRouter does not, each with its
//     reason, so such a field is a reviewed decision (ADR-0059 #2985).
//   - A check that accepts a registered name and an unregistered one: probing
//     the registry sees only the registered one. The source test
//     TestNoAdministrationPermissionLiteralOutsideTheRegistry covers it.
//
// # Never in production
//
// The recording resolver grants whatever its probe asks. Nothing in
// cmd/elitea-main may import this package; a test there fails if the binary
// links it.
package adminsurface
