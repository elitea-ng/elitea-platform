package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// DatasetHandler serves the eight dataset and dataset-case routes.
//
// It is a SEPARATE handler from the dimension one rather than more methods on
// it, because the two take different repositories and the router composes them
// independently: a deployment with a pool has both, and the nil gate on each is
// its own. Merging them would make one nil dependency hide the other feature.
type DatasetHandler struct {
	repo DatasetRepository
}

func NewDatasetHandler(repo DatasetRepository) *DatasetHandler {
	return &DatasetHandler{repo: repo}
}

// datasetListResponse is `{rows, total}` — the same envelope the dimension
// listing uses and every other /elitea_core project listing uses.
//
// The choice is load-bearing and it is pinned on both sides. This API serves
// three list shapes, and a client reading the wrong key gets `undefined`,
// coerces it to `[]`, and renders an empty page behind a 200 with nothing in
// the console (#132). The web client goes through `shared/api/unwrap.ts`'s
// `unwrapList`, which accepts all three and is loud on a fourth.
type datasetListResponse struct {
	Rows  []Dataset `json:"rows"`
	Total int       `json:"total"`
}

func (h *DatasetHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	filter := DatasetListFilter{}
	if raw := r.URL.Query().Get("agent_id"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			apierr.Write(w, apierr.BadRequest("agent_id must be an integer"))
			return
		}
		filter.ApplicationID = &parsed
	}

	datasets, err := h.repo.ListDatasets(r.Context(), projectID, filter)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if datasets == nil {
		datasets = []Dataset{}
	}
	writeJSON(w, http.StatusOK, datasetListResponse{Rows: datasets, Total: len(datasets)})
}

// Get reads ONE dataset with a page of its cases.
func (h *DatasetHandler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	datasetID := chi.URLParam(r, "datasetID")

	page, ok := decodeCasePage(w, r)
	if !ok {
		return
	}

	detail, err := h.repo.GetDataset(r.Context(), projectID, datasetID, page)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	// A dataset with no cases answers `"cases": []`. A missing key reads as
	// "not loaded yet" to a client, which then shows a spinner for ever — see
	// the note on DatasetDetail, which exists for exactly this.
	if detail.Cases == nil {
		detail.Cases = []DatasetCase{}
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *DatasetHandler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var input DatasetWriteInput
	if !decodeStrict(w, r, &input) {
		return
	}
	input.Normalize()
	if err := input.Validate(); err != nil {
		apierr.Write(w, err)
		return
	}

	created, err := h.repo.CreateDataset(r.Context(), projectID, input)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *DatasetHandler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	datasetID := chi.URLParam(r, "datasetID")

	var input DatasetWriteInput
	if !decodeStrict(w, r, &input) {
		return
	}
	input.Normalize()
	if err := input.Validate(); err != nil {
		apierr.Write(w, err)
		return
	}

	updated, err := h.repo.UpdateDataset(r.Context(), projectID, datasetID, input)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *DatasetHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.repo.DeleteDataset(r.Context(),
		chi.URLParam(r, "projectID"), chi.URLParam(r, "datasetID")); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *DatasetHandler) AddCase(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	datasetID := chi.URLParam(r, "datasetID")

	var input CaseWriteInput
	if !decodeStrict(w, r, &input) {
		return
	}
	input.Normalize()
	if err := input.Validate(); err != nil {
		apierr.Write(w, err)
		return
	}

	created, err := h.repo.AddCase(r.Context(), projectID, datasetID, input)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// UpdateCase rewrites one case, or ONLY its exclusion flag.
//
// A body whose single key is `excluded` writes the flag and nothing else. The
// include/exclude checkbox sends exactly that. Before this form existed the
// checkbox had to send the case text back with the flag, i.e. the client's
// cached copy of it, so a toggle from a stale view silently reverted an edit
// made in another tab. Any other body is a full rewrite, and `input` is
// required in it as before.
func (h *DatasetHandler) UpdateCase(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	datasetID := chi.URLParam(r, "datasetID")
	caseID := chi.URLParam(r, "caseID")

	raw, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	if excluded, flagOnly, err := exclusionOnly(raw); err != nil {
		apierr.Write(w, err)
		return
	} else if flagOnly {
		updated, err := h.repo.SetCaseExcluded(r.Context(), projectID, datasetID, caseID, excluded)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}

	var input CaseWriteInput
	if !decodeStrictBytes(w, raw, &input) {
		return
	}
	input.Normalize()
	if err := input.Validate(); err != nil {
		apierr.Write(w, err)
		return
	}

	updated, err := h.repo.UpdateCase(r.Context(), projectID, datasetID, caseID, input)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *DatasetHandler) DeleteCase(w http.ResponseWriter, r *http.Request) {
	if err := h.repo.DeleteCase(r.Context(),
		chi.URLParam(r, "projectID"),
		chi.URLParam(r, "datasetID"),
		chi.URLParam(r, "caseID")); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeCasePage reads `limit` and `offset`, bounded.
//
// An out-of-range limit is REFUSED rather than clamped. A silent clamp answers
// 200 with a page the caller did not ask for, and a client that paged by
// `offset += limit` would then skip rows it never saw.
func decodeCasePage(w http.ResponseWriter, r *http.Request) (CasePage, bool) {
	page := CasePage{Limit: DefaultCasePageLimit}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > MaxCasePageLimit {
			apierr.Write(w, apierr.BadRequest("limit must be a whole number between 1 and 1000"))
			return CasePage{}, false
		}
		page.Limit = parsed
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			apierr.Write(w, apierr.BadRequest("offset must be a whole number of 0 or more"))
			return CasePage{}, false
		}
		page.Offset = parsed
	}
	return page, true
}

// decodeStrict refuses an unknown field rather than dropping it.
//
// Same reasoning as the dimension decoder: accepting and discarding a field
// reports success for a setting that was never stored. The concrete risk here
// is the reference's dataset body, which carries `isShared` in camelCase while
// this API is snake_case throughout — a lenient decoder would store `is_shared`
// false for every dataset a reference-shaped client created, with a 201 each
// time.
//
// The body is read through readBoundedBody first, so an oversized body is a
// 413 before any of it is decoded.
func decodeStrict(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, ok := readBoundedBody(w, r)
	if !ok {
		return false
	}
	return decodeStrictBytes(w, raw, target)
}

func decodeStrictBytes(w http.ResponseWriter, raw []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body: "+err.Error()))
		return false
	}
	return true
}

// readBoundedBody reads the whole request body, refusing one larger than
// MaxWriteBodyBytes with a 413.
//
// Every evaluation write goes through here. The per-field caps in Validate
// run only after decoding, and decoding buffers the whole body: without this
// bound an authenticated editor could stream hundreds of megabytes into the
// process before any cap is looked at.
func readBoundedBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWriteBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apierr.WriteStatus(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body too large: at most %d bytes", MaxWriteBodyBytes))
			return nil, false
		}
		apierr.Write(w, apierr.BadRequest("the request body could not be read"))
		return nil, false
	}
	return raw, true
}

// exclusionOnly reports whether a case update body is the flag-only form,
// `{"excluded": true|false}`, and the flag it carries.
//
// A body that is not a JSON object, or has any other key, is not the flag-only
// form, and the caller decodes it as a full rewrite (which then reports its
// own errors). `{"excluded": null}` is refused: it names the flag and gives it
// no value, and treating it as either value would be a guess.
func exclusionOnly(raw []byte) (bool, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 1 {
		return false, false, nil
	}
	value, present := fields["excluded"]
	if !present {
		return false, false, nil
	}
	var excluded *bool
	if err := json.Unmarshal(value, &excluded); err != nil || excluded == nil {
		return false, false, apierr.BadRequest("excluded must be true or false")
	}
	return *excluded, true, nil
}
