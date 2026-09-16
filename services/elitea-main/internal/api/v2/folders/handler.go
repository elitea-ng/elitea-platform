package folders

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// PositionGap is the spacing between two adjacent folders' positions. It
// matches the legacy runtime's own `POSITION_GAP` (elitea_core/api/v2/
// folder.py:47) — a large gap so ~20 successive "drop between these two"
// halvings fit before two neighbours collide and a rebalance is needed.
const PositionGap = 1_000_000

type Folder struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	ParentID  string `json:"parent_id,omitempty"`
	// Position orders the sidebar, DESCENDING: the highest position renders
	// first. That is the legacy ordering (`order_by(desc(position),
	// created_at)`, folder.py:325-328) and the one the web client's own
	// arithmetic assumes (`computePositionBetweenNeighbors` in
	// useDragAndDrop.positioning.ts:27-32 returns `posBelow + POSITION_GAP`
	// for a drop at the top).
	//
	// A pointer, so an absent `position` in a PUT body is distinguishable
	// from an explicit `0`: a rename PUT (`{"name": …}`, which is all
	// useFolderUpdateMutation sends) must leave the stored order alone.
	Position  *int      `json:"position,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Repository interface {
	List(ctx context.Context, projectID string) ([]Folder, error)
	Create(ctx context.Context, projectID string, folder Folder) (Folder, error)
	Update(ctx context.Context, projectID, folderID string, folder Folder) (Folder, error)
	Delete(ctx context.Context, projectID, folderID string) error
	// Rebalance rewrites every folder's position with fresh PositionGap
	// spacing, preserving the current display order, and returns the
	// folders in their new state. Called only when a requested insertion
	// point has no room left between its neighbours.
	Rebalance(ctx context.Context, projectID string) ([]Folder, error)
}

type Handler struct {
	repo Repository
	pool *pgxpool.Pool
}

func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) WithPool(pool *pgxpool.Pool) *Handler {
	h.pool = pool
	return h
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Put("/{folderID}", h.Update)
	r.Patch("/{folderID}", h.Update)
	r.Delete("/{folderID}", h.Delete)
	return r
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	grouped := r.URL.Query().Get("grouped") == "true"

	// `folder_id` / `date_group` select ONE folder's (or one date group's)
	// page of conversations rather than the sidebar as a whole — the two
	// lazy pagination fetchers in entities/folder/api/foldersApi.ts
	// (`folderConversations`, `dateGroupConversations`). They are checked
	// before `grouped`, because both fetchers send `grouped=true` alongside
	// the filter and neither wants the folder list back.
	if folderID := r.URL.Query().Get("folder_id"); folderID != "" {
		h.listFolderConversations(w, r, projectID, folderID)
		return
	}
	if dateGroup := r.URL.Query().Get("date_group"); dateGroup != "" {
		h.listDateGroupConversations(w, r, projectID, dateGroup)
		return
	}

	if !grouped {
		folders, err := h.repo.List(r.Context(), projectID)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": folders})
		return
	}

	h.listGrouped(w, r, projectID)
}

type conversationItem struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	UUID     string `json:"uuid,omitempty"`
	AuthorID int    `json:"author_id"`
	FolderID *int   `json:"folder_id"`
	IsPinned bool   `json:"is_pinned,omitempty"`
	// `chat_conversations.is_private`. The row menu offers "Make public" only
	// while a conversation is private, and this listing is where the sidebar
	// learns each row's state — the client normaliser already reads the key
	// (`entities/folder/api/foldersApi.ts`, `is_private` → `isPrivate`) and
	// defaults an absent one to private, so the control never withdrew after
	// a conversation was published.
	IsPrivate bool       `json:"is_private"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
}

type dateGroup struct {
	Name          string             `json:"name"`
	Conversations []conversationItem `json:"conversations"`
	// Total is the size of the WHOLE bucket, not of this page. The rail's
	// load-more sentinel mounts only while `total` exceeds the rows it
	// already holds, so a total equal to the delivered count is the same
	// statement as "there is no more" — see firstPage.
	Total int `json:"total"`
	// Offset is where the next page starts: the number of rows this response
	// carries. The client sends it back as `?date_group=…&offset=…`.
	Offset int `json:"offset"`
}

// groupOrder is the sidebar's date-group order, newest bucket first. Also
// the set of names `date_group=` accepts (see normaliseGroupLabel).
var groupOrder = []string{"Today", "Yesterday", "This week", "This month", "Older"}

// loadConversations reads every visible conversation in the project, applying
// the request's sort and `query` filter.
//
// It returns an error rather than an empty slice when the query fails. The
// previous `if err == nil` swallow is exactly what hid #128 defect 2: with no
// pool wired the endpoint reported an empty-but-successful sidebar — nine
// conversations and every folder rendering as empty, indistinguishable from a
// genuinely empty project. A nil pool is still a silent empty result, because
// that is the deliberate degrade for the handler-unit and contract tests that
// construct the handler without one.
func (h *Handler) loadConversations(ctx context.Context, r *http.Request, projectID string) ([]conversationItem, error) {
	conversations := make([]conversationItem, 0)
	if h.pool == nil {
		return conversations, nil
	}

	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, err
	}

	sortBy := r.URL.Query().Get("sort_by")
	if sortBy == "" {
		sortBy = "updated_at"
	}
	sortOrder := r.URL.Query().Get("sort_order")
	if sortOrder == "" {
		sortOrder = "desc"
	}

	// COALESCE, and a tie-break on the primary key.
	//
	// `updated_at` IS NULLABLE and carries no default (tenant migration 0123),
	// so every conversation that has never been revised holds NULL there. Two
	// consequences, both of which reached the screen:
	//
	//  1. `ORDER BY c.updated_at DESC` puts NULLs FIRST in PostgreSQL. A
	//     conversation nobody has touched since it was created therefore
	//     sorted ABOVE one that was edited a minute ago.
	//  2. Every one of those NULLs compares equal, so the relative order of
	//     the whole never-revised set was whatever the scan happened to
	//     return — different on every request. The rail re-sorts what it
	//     receives by `updated_at ?? created_at` descending
	//     (`features/chat-conversation-list/lib/helpers/conversationList.helpers.ts`),
	//     so the answer and the screen disagreed at random, and a reader
	//     comparing the two saw rows "move" between two identical listings.
	//
	// `groupByDate` below already reads `created_at` when `updated_at` is
	// absent; the sort now reads the same value the grouping does, which is
	// what makes "newest first" one statement rather than two. The `c.id`
	// tie-break, in the same direction, makes the answer total: two rows that
	// share a timestamp to the microsecond still come back in a fixed order.
	orderCol := "COALESCE(c.updated_at, c.created_at)"
	switch sortBy {
	case "created_at":
		orderCol = "c.created_at"
	case "name":
		orderCol = "c.name"
	}
	orderDir := "DESC"
	if sortOrder == "asc" {
		orderDir = "ASC"
	}

	access, err := chatauthority.Load(ctx, h.pool, projectID)
	if err != nil {
		return nil, err
	}
	visible, args := access.Predicate(schema, "c", 1, chatauthority.FolderListing)
	// Query conversations (indexes on conversation_id ensure fast joins elsewhere)
	//
	// THE PINNED SET COMES FROM `social_pins`, which is where the pin routes
	// write it (`POST`/`DELETE /social/pin/prompt_lib/{p}/conversation/{id}`,
	// and the `elitea_core/pin` pair beside them). It used to be read from
	// `c.meta->>'is_pinned'`, a key NOTHING in this service or its client ever
	// writes for a conversation — so "Pin on top" answered `{"ok": true}`, the
	// sidebar moved the row optimistically, and the next listing put it back
	// where it was. Legacy reads the same table (elitea_core/api/v2/
	// folder.py:374-380 queries the social Pin model for
	// `entity == 'conversation'`), so this restores the contract rather than
	// inventing one. The `meta` key is kept as a fallback: a row that carries
	// it stays pinned, which costs nothing and cannot unpin anybody.
	//
	q := fmt.Sprintf(`
		SELECT c.id, c.name, COALESCE(c.uuid::text, ''), c.author_id, c.folder_id,
		       (COALESCE((c.meta->>'is_pinned')::boolean, false)
		            OR EXISTS (SELECT 1 FROM centry.social_pins p
		                       WHERE p.entity = 'conversation'
		                         AND p.project_id = $2::text::integer
		                         AND p.entity_id = c.id
		                         AND p.user_id::text = $1)) AS is_pinned,
		       c.is_private,
		       c.created_at, c.updated_at
		FROM %s.chat_conversations c
		WHERE (c.meta->>'is_hidden' IS NULL OR c.meta->>'is_hidden' = 'false') AND %s
		ORDER BY %s %s, c.id %s`, schema, visible, orderCol, orderDir, orderDir)
	args = append(args, projectID)

	rows, err := h.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("folders: list conversations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c conversationItem
		var updatedAt *time.Time
		if err := rows.Scan(&c.ID, &c.Name, &c.UUID, &c.AuthorID, &c.FolderID, &c.IsPinned, &c.IsPrivate, &c.CreatedAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("folders: scan conversation: %w", err)
		}
		c.UpdatedAt = updatedAt
		conversations = append(conversations, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("folders: list conversations: %w", err)
	}

	// Query search filter
	searchQuery := r.URL.Query().Get("query")
	if searchQuery != "" {
		filtered := make([]conversationItem, 0)
		for _, c := range conversations {
			if containsInsensitive(c.Name, searchQuery) {
				filtered = append(filtered, c)
			}
		}
		conversations = filtered
	}

	return conversations, nil
}

// partitionConversations splits a project's conversations the three ways the
// sidebar renders them: pinned, under a folder (keyed by folder id), and
// ungrouped (which is what the date groups are built from).
func partitionConversations(conversations []conversationItem) (pinned []conversationItem, foldered map[int][]conversationItem, ungrouped []conversationItem) {
	pinned = make([]conversationItem, 0)
	foldered = make(map[int][]conversationItem)
	ungrouped = make([]conversationItem, 0)

	for _, c := range conversations {
		switch {
		case c.IsPinned:
			pinned = append(pinned, c)
		case c.FolderID != nil:
			foldered[*c.FolderID] = append(foldered[*c.FolderID], c)
		default:
			ungrouped = append(ungrouped, c)
		}
	}
	return pinned, foldered, ungrouped
}

// groupByDate buckets ungrouped conversations by recency, keyed by the labels
// in groupOrder. Every label is present in the returned map, possibly empty.
func groupByDate(ungrouped []conversationItem, now time.Time) map[string][]conversationItem {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	yesterday := today.AddDate(0, 0, -1)
	weekAgo := today.AddDate(0, 0, -7)
	monthAgo := today.AddDate(0, -1, 0)

	groups := map[string][]conversationItem{}
	for _, label := range groupOrder {
		groups[label] = []conversationItem{}
	}

	for _, c := range ungrouped {
		ts := c.CreatedAt
		if c.UpdatedAt != nil {
			ts = *c.UpdatedAt
		}
		switch {
		case !ts.Before(today):
			groups["Today"] = append(groups["Today"], c)
		case !ts.Before(yesterday):
			groups["Yesterday"] = append(groups["Yesterday"], c)
		case !ts.Before(weekAgo):
			groups["This week"] = append(groups["This week"], c)
		case !ts.Before(monthAgo):
			groups["This month"] = append(groups["This month"], c)
		default:
			groups["Older"] = append(groups["Older"], c)
		}
	}
	return groups
}

// normaliseGroupLabel folds a date-group name to a comparison key, so the
// `date_group=` filter accepts whatever spelling the grouped listing handed
// the client back ("This week") as well as the legacy runtime's wire spelling
// ("this_week", elitea_core/api/v2/folder.py:14).
func normaliseGroupLabel(label string) string {
	out := make([]byte, 0, len(label))
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		case c == ' ' || c == '-':
			out = append(out, '_')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// paginate returns the [offset, offset+limit) window of items, clamped.
func paginate(items []conversationItem, limit, offset int) []conversationItem {
	if offset >= len(items) {
		return []conversationItem{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}

// defaultPageSize is the legacy runtime's own limit (folder.py:132-133) and
// the size of one bucket page in the grouped listing.
const defaultPageSize = 10

// pageParams reads the `limit`/`offset` pair both lazy fetchers send,
// defaulting to the legacy runtime's own limit=10/offset=0 (folder.py:132-133).
func pageParams(r *http.Request) (limit, offset int) {
	limit = defaultPageSize
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}

// firstPage is one bucket's opening page of the GROUPED listing, and the pair
// of counters the client needs to ask for the next one.
//
// DEFECT #852. Every bucket used to be emitted whole, with
// `total = offset = len(conversations)`. `total` is what the rail's
// LoadMoreSentinel compares against the rows it holds
// (`hasMore = totalAvailableCount > listCurrentSize`), so a total that always
// equalled the delivered count made `hasMore` false for every bucket that has
// ever existed: the sentinel never mounted, `onLoadMoreInGroup` never fired,
// and `?date_group=…&offset=…` — a route this handler has served since #128 —
// had no caller. A project whose sidebar was longer than one screen was not
// slow to load; it simply never loaded the rest.
//
// `total` is now the SIZE OF THE BUCKET and `offset` the number of rows this
// response carries, which is where the next page starts. The two are equal
// only when the bucket really did fit in one page, which is the state the old
// code asserted unconditionally.
//
// SEARCH IS NOT PAGED. `query=` filters the whole project before the buckets
// are cut, and the load-more fetchers do not carry the term
// (`dateGroupConversations` sends `date_group`/`limit`/`offset`/`sort_*` and
// nothing else), so a paged search would drop matches on the floor and then
// fetch UNFILTERED rows to replace them. A filtered listing is therefore
// served whole, and its `total` still equals its delivered count — honestly
// this time, because it is the whole answer.
func firstPage(all []conversationItem, pageSize int, paged bool) (page []conversationItem, total, offset int) {
	if !paged {
		return all, len(all), len(all)
	}
	page = paginate(all, pageSize, 0)
	return page, len(all), len(page)
}

// listFolderConversations answers `?folder_id=N` — one folder's page of
// conversations, the shape entities/folder/api/foldersApi.ts's
// `folderConversations` normaliser reads (`conversations`/`total`/`offset`).
// Before #128 the filter was ignored entirely and the folder LIST came back.
func (h *Handler) listFolderConversations(w http.ResponseWriter, r *http.Request, projectID, folderID string) {
	ctx := r.Context()

	folders, err := h.repo.List(ctx, projectID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	found := false
	for _, f := range folders {
		if f.ID == folderID {
			found = true
			break
		}
	}
	if !found {
		apierr.Write(w, apierr.NotFound("folder not found"))
		return
	}

	conversations, err := h.loadConversations(ctx, r, projectID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	wantID, convErr := strconv.Atoi(folderID)
	inFolder := make([]conversationItem, 0)
	if convErr == nil {
		for _, c := range conversations {
			if c.FolderID != nil && *c.FolderID == wantID {
				inFolder = append(inFolder, c)
			}
		}
	}

	limit, offset := pageParams(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"folder_id":     folderID,
		"total":         len(inFolder),
		"limit":         limit,
		"offset":        offset,
		"conversations": paginate(inFolder, limit, offset),
	})
}

// listDateGroupConversations answers `?date_group=Today` — the date-group twin
// of listFolderConversations, backing `dateGroupConversations` in
// entities/folder/api/foldersApi.ts.
func (h *Handler) listDateGroupConversations(w http.ResponseWriter, r *http.Request, projectID, dateGroup string) {
	ctx := r.Context()

	conversations, err := h.loadConversations(ctx, r, projectID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	_, _, ungrouped := partitionConversations(conversations)
	groups := groupByDate(ungrouped, time.Now())

	want := normaliseGroupLabel(dateGroup)
	matched := make([]conversationItem, 0)
	knownGroup := false
	for _, label := range groupOrder {
		if normaliseGroupLabel(label) == want {
			matched = groups[label]
			knownGroup = true
			break
		}
	}
	if !knownGroup {
		apierr.Write(w, apierr.BadRequest("unknown date_group"))
		return
	}

	limit, offset := pageParams(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"date_group":               dateGroup,
		"total":                    len(matched),
		"limit":                    limit,
		"offset":                   offset,
		"selected_conversation_id": h.selectedConversationID(ctx, projectID),
		"conversations":            paginate(matched, limit, offset),
	})
}

func (h *Handler) listGrouped(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	conversations, err := h.loadConversations(ctx, r, projectID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	pinned, foldered, ungrouped := partitionConversations(conversations)
	groups := groupByDate(ungrouped, time.Now())

	// One page per bucket, and a `total` that says how much is behind it.
	// See firstPage for why a filtered listing is served whole.
	pageSize, _ := pageParams(r)
	paged := r.URL.Query().Get("query") == ""

	dateGroups := make([]dateGroup, 0)
	for _, label := range groupOrder {
		convs := groups[label]
		if len(convs) > 0 {
			page, total, offset := firstPage(convs, pageSize, paged)
			dateGroups = append(dateGroups, dateGroup{
				Name:          label,
				Conversations: page,
				Total:         total,
				Offset:        offset,
			})
		}
	}

	// Build folders with conversations
	folders, err := h.repo.List(ctx, projectID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	foldersResponse := make([]map[string]any, 0, len(folders))
	for _, f := range folders {
		fID := 0
		_, _ = fmt.Sscanf(f.ID, "%d", &fID)
		convs := foldered[fID]
		if convs == nil {
			convs = []conversationItem{}
		}
		// A folder pages exactly like a date bucket. Its client half is the
		// same code — `onLoadMoreInFolder` beside `onLoadMoreInGroup`, reading
		// the same `total`/`offset` pair through the same merge helper — and
		// `?folder_id=…&offset=…` is already served, so leaving folders whole
		// here would keep half of #852 alive.
		page, total, offset := firstPage(convs, pageSize, paged)
		row := map[string]any{
			"id":            f.ID,
			"name":          f.Name,
			"project_id":    f.ProjectID,
			"created_at":    f.CreatedAt,
			"updated_at":    f.UpdatedAt,
			"conversations": page,
			"total":         total,
			"offset":        offset,
		}
		if f.Position != nil {
			row["position"] = *f.Position
		}
		foldersResponse = append(foldersResponse, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"date_groups":              dateGroups,
		"folders":                  foldersResponse,
		"pinned":                   map[string]any{"conversations": pinned},
		"selected_conversation_id": h.selectedConversationID(ctx, projectID),
		"total_folders":            len(folders),
	})
}

// selectedConversationID reads the caller's currently-selected conversation,
// or nil when there is none (or no pool / no authenticated user). A miss here
// is not an error: an unselected sidebar is the normal first-visit state.
func (h *Handler) selectedConversationID(ctx context.Context, projectID string) *int {
	if h.pool == nil {
		return nil
	}
	access, err := chatauthority.Load(ctx, h.pool, projectID)
	if err != nil {
		return nil
	}
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil
	}
	visibility, args := access.Predicate(schema, "c", 1, chatauthority.FolderListing)
	q := fmt.Sprintf(`SELECT selected.conversation_id FROM %s.chat_selected_conversations selected JOIN %s.chat_conversations c ON c.id=selected.conversation_id WHERE selected.user_id=$1 AND %s LIMIT 1`, schema, schema, visibility)
	var selID int
	if err := h.pool.QueryRow(ctx, q, args...).Scan(&selID); err != nil {
		return nil
	}
	return &selID
}

func containsInsensitive(s, substr string) bool {
	sl := len(substr)
	if sl == 0 {
		return true
	}
	for i := 0; i <= len(s)-sl; i++ {
		if eqFoldByte(s[i:i+sl], substr) {
			return true
		}
	}
	return false
}

func eqFoldByte(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	folders, err := h.repo.List(r.Context(), projectID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	folderID := chi.URLParam(r, "folderID")
	for _, f := range folders {
		if f.ID == folderID {
			writeJSON(w, http.StatusOK, f)
			return
		}
	}
	apierr.Write(w, apierr.NotFound("folder not found"))
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var folder Folder
	if err := json.NewDecoder(r.Body).Decode(&folder); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	created, err := h.repo.Create(r.Context(), projectID, folder)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	folderID := chi.URLParam(r, "folderID")

	// PATCH: partial merge — fetch existing folder, apply only provided fields.
	if r.Method == http.MethodPatch {
		folders, err := h.repo.List(r.Context(), projectID)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		var existing *Folder
		for i := range folders {
			if folders[i].ID == folderID {
				existing = &folders[i]
				break
			}
		}
		if existing == nil {
			apierr.Write(w, apierr.NotFound("folder not found"))
			return
		}

		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			apierr.Write(w, apierr.BadRequest("invalid request body"))
			return
		}
		if name, ok := patch["name"].(string); ok {
			existing.Name = name
		}
		if parentID, ok := patch["parent_id"].(string); ok {
			existing.ParentID = parentID
		}

		updated, err := h.repo.Update(r.Context(), projectID, folderID, *existing)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}

	// PUT: full replacement, plus the reorder payload useReorderFolders.ts:79-97
	// sends alongside the name.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	var folder Folder
	if err := json.Unmarshal(body, &folder); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	if err := h.resolveReorder(r.Context(), projectID, folderID, raw, &folder); err != nil {
		apierr.Write(w, err)
		return
	}

	updated, err := h.repo.Update(r.Context(), projectID, folderID, folder)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// resolveReorder turns a drag-and-drop payload into the absolute position the
// folder should be stored at, mutating folder.Position in place.
//
// The two inputs are `position` (the client's own arithmetic) and the
// neighbour pair (`neighbor_above_id`/`neighbor_below_id`, the ids the folder
// was dropped between). **Neighbours win when present.** They state the drop
// intent exactly, whereas `position` is a number computed against the
// client's possibly-stale copy of the list — and the two disagree the moment
// another client has reordered anything. The legacy runtime consulted the
// neighbours only after a collision forced a rebalance (folder.py:697-745),
// which is why a drag there could return 200 and silently not move the
// folder. Deliberate deviation, and the reason `neighbor_below_id` alone is
// enough to move a folder to the top.
//
// With no neighbours, an explicit `position` is stored verbatim. With
// neither, the stored order is left untouched — that is the rename path.
func (h *Handler) resolveReorder(ctx context.Context, projectID, folderID string, raw map[string]json.RawMessage, folder *Folder) error {
	aboveID, hasAbove := optionalID(raw["neighbor_above_id"])
	belowID, hasBelow := optionalID(raw["neighbor_below_id"])
	if !hasAbove && !hasBelow {
		// folder.Position already carries the decoded `position`, or nil.
		return nil
	}

	folders, err := h.repo.List(ctx, projectID)
	if err != nil {
		return err
	}

	pos, needsRebalance := positionBetween(folders, aboveID, belowID)
	if needsRebalance {
		folders, err = h.repo.Rebalance(ctx, projectID)
		if err != nil {
			return err
		}
		pos, _ = positionBetween(folders, aboveID, belowID)
	}
	folder.Position = &pos
	return nil
}

// optionalID reads a neighbour id that the client sends as `null` when the
// folder was dropped at an end of the list. A JSON `null`, an absent key and
// an empty string all mean "no neighbour on this side".
func optionalID(rawID json.RawMessage) (string, bool) {
	if len(rawID) == 0 || string(rawID) == "null" {
		return "", false
	}
	var asString string
	if err := json.Unmarshal(rawID, &asString); err == nil {
		return asString, asString != ""
	}
	var asNumber json.Number
	if err := json.Unmarshal(rawID, &asNumber); err == nil {
		return asNumber.String(), true
	}
	return "", false
}

// positionBetween computes the position that lands a folder between the two
// named neighbours, remembering that positions sort DESCENDING: the folder
// displayed *above* holds the *higher* position.
//
// It reports needsRebalance when the two neighbours are already adjacent
// integers, leaving no value strictly between them.
func positionBetween(folders []Folder, aboveID, belowID string) (position int, needsRebalance bool) {
	var above, below *int
	for i := range folders {
		if folders[i].Position == nil {
			continue
		}
		switch folders[i].ID {
		case aboveID:
			above = folders[i].Position
		case belowID:
			below = folders[i].Position
		}
	}

	switch {
	case above == nil && below == nil:
		return PositionGap, false
	case above == nil: // dropped at the top: sit above the folder below it
		return *below + PositionGap, false
	case below == nil: // dropped at the bottom: sit below the folder above it
		return *above / 2, *above <= 1
	default:
		if *above-*below <= 1 {
			return *below, true
		}
		return (*above + *below) / 2, false
	}
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	folderID := chi.URLParam(r, "folderID")

	if err := h.repo.Delete(r.Context(), projectID, folderID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
