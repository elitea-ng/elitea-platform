// Package personalproject creates the per-user "private" project every account
// needs before the product is usable, and that this stack never created.
//
// THE DEFECT IT CLOSES. `GET /social/author` answers `personal_project_id`, and
// internal/api/v2/social/handler.go's resolvePersonalProjectID said so in its
// own doc comment: "this Go stack never PROVISIONS a `project_user_<uid>`
// project, so branch 1 only ever fires for data migrated from pylon". On a
// deployment with no pylon data behind it — every fresh install — that value is
// therefore `""` for every account, forever. The SPA reads it as "no personal
// project yet" and sends the browser to `/onboarding`
// (apps/elitea-web/src/routes/-guards/indexRoute.ts), which waits for a project
// that nothing was ever going to create. Every new user was stuck on that
// screen.
//
// THE REFERENCE is legacy/plugins/projects: `create_personal_project`
// (rpc/poc.py:64) names the project `project_user_<uid>` and provisions it
// through the same create-project pipeline an ordinary project uses, and
// methods/private_projects.py runs it OFF the request path — pylon's auth layer
// fires an `auth_visitor` event for every authenticated request, the projects
// plugin queues the visitor, and a background thread provisions the project
// while the SPA polls `/social/author` every five seconds until the id appears.
// The onboarding screen's "Configuring Personal project… about 5 min" is that
// wait.
//
// WHAT IS PORTED, AND WHAT IS NOT.
//
//   - The name, the ownership, and the off-request-path execution are ported.
//   - The queue and its worker thread are NOT. Ensure is deduplicated per user
//     inside the process and serialized across processes by a PostgreSQL
//     advisory lock, which is what the queue was doing on a single-process
//     runtime and is what a multi-replica deployment actually needs.
//   - pylon's `plugins=('configuration', 'models')` is kept verbatim. Nothing
//     in this service reads centry.project.plugins — it is echoed back by the
//     project listings and nothing else — so the value is parity rather than
//     behaviour. It is what a pylon-created personal project carries.
//   - pylon grants the owner `editor`, `viewer` and `monitor` on their own
//     project. This grants `admin`, which is what projectprovisioning already
//     gives the maker of any project whose request names no administrator
//     (steps.go's ownerProjectRole). A personal project's owner is the only
//     person who can administer it, and `monitor` no longer exists
//     (auth_core's 202602231400_remove_monitor_role migration).
package personalproject

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
)

// Provisioner is the create/delete half of projectprovisioning.Provisioner this
// package needs, declared at the consumer so the ensurer can be tested without
// the tenant migration corpus.
//
// Deprovision is part of it on purpose. See Ensure's repair branch: a project
// row left behind with `create_success = false` is a personal project its owner
// can never be given, and pylon's own `fix_create_personal_projects` repairs
// exactly that state by deleting and recreating.
type Provisioner interface {
	Provision(ctx context.Context, request projectprovisioning.Request) (projectprovisioning.Result, error)
	Deprovision(ctx context.Context, projectID int64) (projectprovisioning.Result, error)
}

// AsyncEnsurer is the half of *Ensurer a request path holds: ask for the
// caller's personal project, do not wait for it.
//
// Declared here rather than again at each consumer. `/social/author` and
// api/middleware's project resolver are the two readers of "the caller's
// personal project" and both need exactly this one method, so a copy in each
// bought nothing and cost two doc comments that had to keep agreeing. A nil
// value is a working no-op, which is why neither consumer branches on it.
type AsyncEnsurer interface {
	EnsureAsync(userID int64)
}

// NamePrefix is pylon's PROJECT_PERSONAL_NAME_TEMPLATE
// (legacy/plugins/projects/constants.py:13) without its id.
//
// IT IS EXPORTED BECAUSE THE WRITER AND THE READERS MUST AGREE. This package
// writes the name; social/handler.go's resolvePersonalProjectID and
// api/middleware/project_resolver.go read it. Each used to carry its own
// `project_user_` literal, and nothing failed if one changed — a rename that
// updated the writer alone would make provisioning succeed and resolution
// answer "" for every account, which is the exact defect this package exists
// to close. Both readers now build the name from here.
const NamePrefix = "project_user_"

// Name is the personal project of one user, by id.
func Name(userID int64) string {
	return NamePrefix + strconv.FormatInt(userID, 10)
}

// personalProjectPlugins is pylon's create_personal_project default
// (legacy/plugins/projects/rpc/poc.py:64). See the package comment.
var personalProjectPlugins = []string{"configuration", "models"}

// systemUserEmail matches the per-project system identity pylon creates for
// every project (`system_user_<n>@centry.user`,
// legacy/plugins/projects/constants.py:11). Those accounts already resolve a
// personal project id — it is the second branch of resolvePersonalProjectID —
// and giving one a project of its own would create a project per project.
var systemUserEmail = regexp.MustCompile(`^system_user_[0-9]+@centry\.user$`)

// systemUserNamePrefix is PROJECT_USER_NAME_PREFIX. private_projects.py skips a
// visitor whose name starts with it, so this does too.
const systemUserNamePrefix = ":system:project:"

// advisoryLockClass namespaces this package's advisory locks. PostgreSQL
// advisory locks share one global space keyed by two int4s, so the class keeps
// a lock on user 9 here from colliding with a lock on 9 taken for anything
// else.
//
// The value is arbitrary and permanent. Changing it would let an old process
// and a new one provision the same personal project at the same time during a
// rolling deploy.
const advisoryLockClass = 0x454C5041 // "ELPA"

// maxConcurrentProvisions bounds how many personal projects this process
// builds at once.
//
// IT IS A CONNECTION BUDGET, not a throughput knob. Ensure holds one pool
// connection for its whole run — the advisory lock is session scoped, so it
// cannot be released earlier — while projectprovisioning takes FURTHER
// connections from that same pool for its steps, and migrate.Runner.ApplyTenant
// takes one of its own. The pool defaults to max(4, NumCPU) connections, so on
// a small pod four simultaneous first logins would hold every connection for
// their locks and then block forever waiting for a connection to do the work
// with — starving the whole API until the timeout fired.
//
// One, matching the reference: pylon's private_projects.py runs a SINGLE
// worker thread over a visitor queue. Waiting is free here, because nothing is
// waiting on the result — the SPA polls.
const maxConcurrentProvisions = 1

// defaultProvisionTimeout bounds one provisioning attempt.
//
// It is generous because the work is genuinely long: the project_schema step
// applies the entire tenant migration corpus to a new schema. It runs detached
// from the request that triggered it, so nothing is waiting on it.
const defaultProvisionTimeout = 10 * time.Minute

// ErrNotConfigured reports an ensurer built without its dependencies.
var ErrNotConfigured = errors.New("personalproject: ensurer is not configured")

// Ensurer creates the personal project of one user, once.
type Ensurer struct {
	pool        *pgxpool.Pool
	provisioner Provisioner
	logger      *slog.Logger
	timeout     time.Duration

	// inFlight maps each user id being provisioned by THIS process to the
	// channel that attempt closes when it ends. The SPA polls
	// `/social/author` every five seconds while it waits, and every one of
	// those polls resolves an empty personal project id and asks again —
	// without this, each poll would start another provisioning attempt, and
	// they would queue up on the advisory lock behind the first.
	//
	// IT HOLDS THE CHANNEL, NOT A MARKER, so a second caller for the same user
	// can JOIN the attempt instead of only learning that it exists. Every
	// value stored here is a `chan struct{}` that some code path closes: the
	// attempt's goroutine when it ends, or EnsureStarted itself when a full
	// slot budget drops the attempt before any goroutine exists. A value that
	// nothing closes would park every later caller for this user until their
	// own deadline expired.
	inFlight sync.Map

	// slots is the concurrency budget maxConcurrentProvisions describes.
	//
	// NewEnsurer is the only thing that makes it, and both entry points check
	// it: `Ensurer` is exported with unexported fields, so `&Ensurer{}` compiles
	// outside this package, and a send on its nil channel would block forever —
	// leaking a goroutine and leaving that user marked in-flight for good.
	slots chan struct{}

	// attempt runs ONE provisioning attempt for one user. It is Ensure for
	// every ensurer NewEnsurer builds.
	//
	// IT IS THE ONE SEAM A TEST REPLACES. What EnsureStarted has to get right
	// is pure concurrency — who starts the attempt, who joins it, who is
	// dropped — and none of that is about PostgreSQL. Driving it through the
	// real Ensure would need a tenant migration corpus and would turn a
	// deterministic test into a timing race against it. The postgres suite
	// beside this file covers Ensure itself.
	attempt func(ctx context.Context, userID int64) (int64, error)
}

// Option configures an Ensurer at construction time.
type Option func(*Ensurer)

// WithLogger replaces the default logger.
func WithLogger(logger *slog.Logger) Option {
	return func(e *Ensurer) {
		if logger != nil {
			e.logger = logger
		}
	}
}

// WithProvisionTimeout replaces the per-attempt timeout. Zero and negative
// values are ignored, so a caller cannot accidentally build an ensurer whose
// every attempt is already expired.
func WithProvisionTimeout(timeout time.Duration) Option {
	return func(e *Ensurer) {
		if timeout > 0 {
			e.timeout = timeout
		}
	}
}

// NewEnsurer builds an Ensurer. Both dependencies are required: without the
// pool it cannot decide whether a project already exists, and without the
// provisioner it cannot create one.
func NewEnsurer(pool *pgxpool.Pool, provisioner Provisioner, options ...Option) (*Ensurer, error) {
	if pool == nil || provisioner == nil {
		return nil, ErrNotConfigured
	}
	ensurer := &Ensurer{
		pool:        pool,
		provisioner: provisioner,
		logger:      slog.Default(),
		timeout:     defaultProvisionTimeout,
		slots:       make(chan struct{}, maxConcurrentProvisions),
	}
	ensurer.attempt = ensurer.Ensure
	for _, option := range options {
		option(ensurer)
	}
	return ensurer, nil
}

// EnsureAsync provisions the user's personal project in the background and
// returns immediately.
//
// It is the shape every caller wants, and the only one a request handler may
// use: provisioning applies a migration corpus, so a handler that waited for it
// would hold a request open for as long as that takes. The caller learns the
// outcome the same way the SPA does — by asking for the personal project id
// again.
//
// IT DROPS RATHER THAN QUEUES. The slot is taken here, before any goroutine
// exists, and a full budget abandons the attempt instead of waiting for one.
// That is safe precisely because nothing is waiting on the result: the SPA
// polls `/social/author` every five seconds and the dropped attempt is simply
// made again on the next poll. Blocking instead would let a burst of first
// logins pile fifty goroutines onto a one-deep budget, each holding its
// in-flight marker — and therefore suppressing its own later polls — while it
// waited tens of minutes for its turn.
//
// A nil receiver, and an Ensurer built without NewEnsurer, are both no-ops, so
// a composition that could not build one needs no branch at the call site.
func (e *Ensurer) EnsureAsync(userID int64) {
	_ = e.EnsureStarted(userID)
}

// EnsureStarted is EnsureAsync with a completion signal.
//
// It returns a channel that is CLOSED when the attempt for this user has
// finished — the one this call started, OR the one that was already running
// when this call arrived. It returns nil only when there is NO attempt to wait
// for: a nil or unconfigured ensurer, or a full slot budget.
//
// A CONCURRENT CALLER JOINS RATHER THAN GETTING NOTHING. The SPA sends two
// `GET /social/author` requests at boot, inside the same second. The first
// starts the provisioning and waits for it; the second used to find the
// in-flight marker, receive nil, and answer `personal_project_id: ""` while
// the first answered the real id. One boot, two contradictory answers, and the
// SPA routes on that field — so which of the two won decided whether a
// first-time user landed on the product or on `/onboarding`. The two callers
// now wait on the SAME channel and read the same world after it closes.
//
// THE CHANNEL BOUNDS THE WAIT, NEVER THE WORK. The goroutine keeps the
// detached `context.Background()` deadline EnsureAsync always gave it, so a
// caller that stops waiting does not cancel the provisioning. Cancelling it
// would leave a `create_success = false` row for the next attempt's repair
// branch to clean up, on every first login, which is worse than the wait it
// saves. A joiner cannot cancel it either: it holds the receive end only.
//
// The close is the LAST deferred action, after the in-flight entry is
// released and the slot is returned. A caller woken by this channel therefore
// re-reads a world where nothing about this user is still pending.
//
// A DROPPED ATTEMPT IS STILL NOT JOINABLE FOREVER. The slot is taken before
// any goroutine exists, so a full budget abandons the attempt — and it closes
// the channel it just published on the way out. A caller that joined in that
// window is released immediately and falls back to the poll, instead of
// waiting for a goroutine that was never started.
//
// It exists for `GET /social/author`, which is both the endpoint that observes
// a missing personal project and the answer the SPA routes on: answering ""
// on the very request that provisions the project sends a first-time user to
// the onboarding screen for a project that already exists.
func (e *Ensurer) EnsureStarted(userID int64) <-chan struct{} {
	if e == nil || e.slots == nil || e.attempt == nil {
		return nil
	}
	// The channel is made BEFORE the store, because the store publishes it:
	// LoadOrStore is the single point at which this call learns whether it owns
	// the attempt for this user or joins one.
	done := make(chan struct{})
	if running, joined := e.inFlight.LoadOrStore(userID, done); joined {
		// Somebody else owns the attempt. Wait on THEIR channel, which their
		// goroutine closes — or which their drop path already closed.
		existing, ok := running.(chan struct{})
		if !ok {
			// Unreachable: this method is the only writer, and it stores
			// nothing else. Answering nil keeps a corrupted map from parking a
			// request handler for its whole wait.
			return nil
		}
		return existing
	}
	select {
	case e.slots <- struct{}{}:
	default:
		// The budget is full. Release the in-flight entry so the caller's next
		// poll is a fresh attempt rather than a no-op against a user nobody is
		// provisioning — and THEN close, in that order, so a joiner woken by
		// the close cannot find the entry of an attempt that no longer exists.
		e.inFlight.Delete(userID)
		close(done)
		return nil
	}
	go func() {
		defer close(done)
		defer e.inFlight.Delete(userID)
		defer func() { <-e.slots }()
		// context.Background(), not the request's: the request that triggered
		// this is answered long before provisioning finishes, and cancelling
		// halfway through leaves a half-built tenant for the next attempt to
		// repair.
		ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
		defer cancel()
		if _, err := e.attempt(ctx, userID); err != nil {
			e.logger.ErrorContext(ctx, "personal project provisioning failed",
				"user_id", userID, "err", err)
		}
	}()
	return done
}

// Ensure returns the id of the user's personal project, creating it if there is
// none.
//
// It answers 0 with a nil error for an identity that must NOT have a personal
// project — a missing, suspended, or system account. That is not a failure: it
// is the same answer resolvePersonalProjectID gives, and the same skip
// private_projects.py's process_visitor makes.
func (e *Ensurer) Ensure(ctx context.Context, userID int64) (int64, error) {
	if e == nil || e.pool == nil || e.provisioner == nil || e.slots == nil {
		return 0, ErrNotConfigured
	}
	if userID <= 0 {
		return 0, fmt.Errorf("personalproject: user id must be positive, got %d", userID)
	}
	// `auth_core__user.id` is an `integer`, and an advisory lock key is an
	// int4. An id outside that range names no account, so it is refused here
	// rather than silently truncated into somebody else's lock.
	if userID > math.MaxInt32 {
		return 0, fmt.Errorf("personalproject: user id %d is out of range", userID)
	}
	// Narrowed ONCE, immediately after the bound check, and passed on as an
	// int32 from here. The deferred unlock below closes over this value: a
	// conversion written inside that closure would not be dominated by the
	// check above — which is exactly what CodeQL's
	// go/incorrect-integer-conversion flagged.
	accountKey := int32(userID)

	eligible, err := e.eligible(ctx, accountKey)
	if err != nil {
		return 0, err
	}
	if !eligible {
		return 0, nil
	}

	// The lock is held for the whole check-and-create, and it is SESSION
	// scoped rather than transaction scoped because provisioning is not one
	// transaction: projectprovisioning commits the project row before it can
	// apply the tenant corpus to it (see that package's doc comment), so a
	// transaction-scoped lock would be released by the first commit and two
	// replicas would each create a `project_user_<uid>` row. centry.project has
	// no unique index on `name`, so nothing else would stop them.
	connection, err := e.pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("personalproject: acquire connection: %w", err)
	}
	defer connection.Release()

	var locked bool
	if err := connection.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1::integer, $2::integer)`,
		int32(advisoryLockClass), accountKey,
	).Scan(&locked); err != nil {
		return 0, fmt.Errorf("personalproject: lock user %d: %w", userID, err)
	}
	if !locked {
		// Another process is provisioning this user's project right now.
		// Waiting would pin this connection for as long as a tenant migration
		// takes; the caller polls, so returning "not ready yet" is both cheaper
		// and truthful.
		return 0, nil
	}
	defer func() {
		// context.WithoutCancel: the unlock must run even when ctx has expired,
		// otherwise the lock lives until the connection is closed and every
		// later attempt reports "someone else is provisioning".
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, unlockErr := connection.Exec(unlockCtx,
			`SELECT pg_advisory_unlock($1::integer, $2::integer)`,
			int32(advisoryLockClass), accountKey,
		); unlockErr != nil {
			e.logger.WarnContext(ctx, "personal project advisory lock was not released",
				"user_id", userID, "err", unlockErr)
		}
	}()

	return e.ensureLocked(ctx, userID, accountKey)
}

// ensureLocked is Ensure's body, with the advisory lock held.
//
// It takes the id BOTH ways on purpose. `accountKey` is the narrowed value the
// bound check above produced, and every query below binds that rather than
// narrowing again: a conversion written here would sit in a function the guard
// does not dominate, which is the shape CodeQL's
// go/incorrect-integer-conversion flagged in the deferred unlock. `userID` is
// what the provisioner's own int64 fields and the project name are built from.
func (e *Ensurer) ensureLocked(ctx context.Context, userID int64, accountKey int32) (int64, error) {
	candidates, err := e.existingProjects(ctx, userID, accountKey)
	if err != nil {
		return 0, err
	}

	// THE ONLY ROW THAT COUNTS IS THE ONE THE RESOLVER WOULD RETURN.
	// resolvePersonalProjectID's first branch matches the name AND requires the
	// caller to hold a project role in it, so a row this caller is not a member
	// of is not their personal project however much its name looks like one.
	// Returning such a row here reported success while /social/author went on
	// answering "" for good.
	for _, candidate := range candidates {
		if candidate.member && candidate.created {
			return candidate.id, nil
		}
	}

	// A row this caller OWNS that provisioning never finished.
	// projectprovisioning compensates its own failures, so reaching this state
	// means the compensation itself failed, or the process died mid-flight.
	// pylon repairs it the same way (fix_create_personal_projects: delete then
	// create).
	//
	// The ownership test is the guard, not decoration: `centry.project.name` is
	// caller-supplied free text on `POST /projects` and carries no unique
	// index, so without it this branch would delete a project somebody else
	// made and named `project_user_<this caller>`.
	// EVERY such row, not the first: existingProjects reads them all because
	// duplicates are possible, and a survivor would still be RETURNED by
	// resolvePersonalProjectID — its first branch matches name and membership
	// and does not filter on create_success, so a member row with a lower id
	// than the project provisioned below would be reported to the SPA as a
	// project whose `p_<id>` schema does not exist.
	remaining := candidates[:0:0]
	for _, candidate := range candidates {
		if candidate.owned && !candidate.created {
			e.logger.WarnContext(ctx, "removing an unfinished personal project before recreating it",
				"user_id", userID, "project_id", candidate.id)
			if _, err := e.provisioner.Deprovision(ctx, candidate.id); err != nil {
				return 0, fmt.Errorf("personalproject: remove unfinished project %d: %w", candidate.id, err)
			}
			continue
		}
		remaining = append(remaining, candidate)
	}

	// Anything STILL STANDING under this name belongs to somebody else. It is
	// left alone and a new row is provisioned beside it: the resolver selects
	// on membership, so the row created below is the one it will answer with,
	// and the alternative is leaving this caller with no personal project at
	// all for as long as the other row exists.
	//
	// Gated on the survivors rather than on how many rows were READ: the
	// ordinary self-repair path above has already deleted the caller's own
	// unfinished row, and reporting a name squatter that does not exist sends
	// an operator looking for one.
	if len(remaining) != 0 {
		e.logger.WarnContext(ctx, "a project of this name exists that the caller is not a member of",
			"user_id", userID, "name", Name(userID), "existing", candidateIDs(remaining))
	}

	result, err := e.provisioner.Provision(ctx, projectprovisioning.Request{
		Name:    Name(userID),
		Plugins: personalProjectPlugins,
		OwnerID: userID,
		// No AdminEmails and no AdminRoles: projectprovisioning's project_admin
		// step then makes the OWNER the project's administrator, which is the
		// grant a personal project wants. See the package comment.
		Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		return 0, fmt.Errorf("personalproject: provision for user %d: %w", userID, err)
	}
	e.logger.InfoContext(ctx, "personal project created",
		"user_id", userID, "project_id", result.ProjectID)
	return result.ProjectID, nil
}

// existingCandidate is one row already carrying this user's personal-project
// name, with the two facts that decide what may be done with it.
type existingCandidate struct {
	id      int64
	created bool
	// owned reports `owner_id = <this user>`. Only an owned row may be deleted.
	owned bool
	// member reports the project-role assignment resolvePersonalProjectID's
	// first branch requires. Only a member row may be RETURNED, because only a
	// member row is one that resolver will ever answer with.
	member bool
}

// existingProjects reads EVERY row already carrying this user's
// personal-project name, lowest id first.
//
// Every row, not the lowest one: `centry.project` has no unique index on
// `name` and `POST /projects` accepts the name as free text, so more than one
// can exist. The caller decides between them with the resolver's own rule
// rather than with a `LIMIT 1` that can pick a row the resolver ignores.
//
// Lowest id first because that is the resolver's tie-break WITHIN its first
// branch, so the row this returns first among the member rows is the row
// `/social/author` will report.
func (e *Ensurer) existingProjects(
	ctx context.Context, userID int64, accountKey int32,
) ([]existingCandidate, error) {
	rows, err := e.pool.Query(ctx, `
SELECT
    project.id,
    project.create_success,
    project.owner_id = $2::integer AS owned,
    EXISTS (
        SELECT 1
        FROM public.auth_core__project_user_role AS assignment
        WHERE assignment.project_id = project.id
          AND assignment.user_id = $2::integer
    ) AS member
FROM centry.project AS project
WHERE project.name = $1
ORDER BY project.id`, Name(userID), accountKey)
	if err != nil {
		return nil, fmt.Errorf("personalproject: read projects for user %d: %w", userID, err)
	}
	defer rows.Close()

	var candidates []existingCandidate
	for rows.Next() {
		var candidate existingCandidate
		if err := rows.Scan(&candidate.id, &candidate.created, &candidate.owned, &candidate.member); err != nil {
			return nil, fmt.Errorf("personalproject: read projects for user %d: %w", userID, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("personalproject: read projects for user %d: %w", userID, err)
	}
	return candidates, nil
}

// candidateIDs renders the ids for the log line that reports a name collision.
func candidateIDs(candidates []existingCandidate) string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, strconv.FormatInt(candidate.id, 10))
	}
	return strings.Join(ids, ",")
}

// eligible reports whether this account is one that should own a personal
// project.
//
// Three accounts are excluded, and each exclusion is pylon's:
//
//   - one that does not exist. Nothing to own a project.
//   - a suspended one. Every other provisioning path in this service refuses a
//     suspended account (identityrepo, and projectprovisioning's own membership
//     inserts filter on `suspended = false`), so creating a project for one
//     would build a tenant its owner cannot reach.
//   - a per-project system identity. process_visitor skips it by name, and
//     resolvePersonalProjectID already answers for it by email.
func (e *Ensurer) eligible(ctx context.Context, accountKey int32) (bool, error) {
	var email, name string
	var suspended bool
	err := e.pool.QueryRow(ctx,
		`SELECT COALESCE(email, ''), COALESCE(name, ''), suspended
		 FROM public.auth_core__user WHERE id = $1`,
		accountKey,
	).Scan(&email, &name, &suspended)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("personalproject: read user %d: %w", accountKey, err)
	}
	if suspended {
		return false, nil
	}
	if systemUserEmail.MatchString(email) || strings.HasPrefix(name, systemUserNamePrefix) {
		return false, nil
	}
	return true, nil
}
