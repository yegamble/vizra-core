// Package search is the frozen core<->search boundary of Q-001. It exists in
// M0 so that the seam is fixed before any product table does, not because
// search is useful yet.
//
// Three implementations, one interface:
//
//   - sql    — permanent. PostgreSQL FTS/trigram, permission-aware. In M0 it
//     returns empty results, because no product tables exist yet.
//   - remote — the HMAC client for vizra-search.
//   - Service — the composition. It is what callers hold.
//
// The distinction Q-001 forbids collapsing: an explicit `not_indexed` status is
// a SUCCESSFUL answer meaning "use your SQL path", and leaves health ok. A
// transport error, a 5xx, a rejected signature or a timeout is a FAULT: the
// request is still served from SQL, but health becomes degraded, readiness says
// so, and `vizra doctor` FAILs. A silent fallback — where a broken search looks
// exactly like a working one — is the failure this split prevents.
package search

import (
	"context"
	"time"

	"github.com/yegamble/vizra-core/internal/authz"
)

// Status mirrors the IndexStatus of api/search-internal.openapi.yaml.
type Status string

const (
	StatusOK         Status = "ok"
	StatusNotIndexed Status = "not_indexed"
)

// Health is what readiness reports for the search component: off | ok |
// degraded (ADR-002 § M0 obligations).
type Health string

const (
	HealthOff      Health = "off"
	HealthOK       Health = "ok"
	HealthDegraded Health = "degraded"
)

// Viewer is the authoritative projection context sent to vizra-search. It is
// derived from the same Subject the evaluator sees, so a projection can never
// be computed from a different identity than the authorization decision.
type Viewer struct {
	UserID      string      `json:"user_id,omitempty"`
	IsAnonymous bool        `json:"is_anonymous"`
	Role        authz.Role  `json:"role"`
	Scope       authz.Scope `json:"scope,omitempty"`
}

// ViewerFrom builds a Viewer from an authorization Subject and scope.
func ViewerFrom(s authz.Subject, scope authz.Scope) Viewer {
	role := s.Role
	if role == "" {
		role = authz.RoleAnonymous
	}
	return Viewer{
		UserID:      s.UserID,
		IsAnonymous: s.Anonymous(),
		Role:        role,
		Scope:       scope,
	}
}

// Site identifies which site is asking (ADR-007 seam).
type Site struct {
	Handle  string `json:"handle"`
	BaseURL string `json:"base_url,omitempty"`
}

// Filters mirrors SearchFilters in the contract.
type Filters struct {
	Tags        []string   `json:"tags,omitempty"`
	Categories  []string   `json:"categories,omitempty"`
	OwnerIDs    []string   `json:"owner_ids,omitempty"`
	Licenses    []string   `json:"licenses,omitempty"`
	AlbumID     string     `json:"album_id,omitempty"`
	CameraMake  string     `json:"camera_make,omitempty"`
	CameraModel string     `json:"camera_model,omitempty"`
	TakenFrom   *time.Time `json:"taken_from,omitempty"`
	TakenTo     *time.Time `json:"taken_to,omitempty"`
	Safety      string     `json:"safety,omitempty"`
}

// SearchRequest mirrors the contract operation body.
type SearchRequest struct {
	Query         string   `json:"query"`
	Viewer        Viewer   `json:"viewer"`
	Site          Site     `json:"site"`
	Filters       *Filters `json:"filters,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	Offset        int      `json:"offset,omitempty"`
	Sort          string   `json:"sort,omitempty"`
	CorrelationID string   `json:"correlation_id,omitempty"`
}

// Hit is one result.
type Hit struct {
	AssetID   string  `json:"asset_id"`
	PublicKey string  `json:"public_key"`
	Score     float64 `json:"score,omitempty"`
}

// SearchResult is the answer. Results is never nil, so a caller that forgets to
// check Status still ranges over an empty slice instead of panicking.
type SearchResult struct {
	Status              Status `json:"status"`
	Results             []Hit  `json:"results"`
	Total               int64  `json:"total"`
	TookMS              int    `json:"took_ms,omitempty"`
	SearchSchemaVersion *int64 `json:"search_schema_version,omitempty"`
}

// SuggestRequest mirrors the contract operation body.
type SuggestRequest struct {
	Prefix        string `json:"prefix"`
	Viewer        Viewer `json:"viewer"`
	Site          Site   `json:"site"`
	Limit         int    `json:"limit,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Suggestion is one autosuggest entry. Count is the number of matching items
// THIS viewer may see; never a global count (matrix row 5).
type Suggestion struct {
	Text  string `json:"text"`
	Kind  string `json:"kind"`
	Count int64  `json:"count,omitempty"`
}

// SuggestResult is the answer.
type SuggestResult struct {
	Status      Status       `json:"status"`
	Suggestions []Suggestion `json:"suggestions"`
	TookMS      int          `json:"took_ms,omitempty"`
}

// Event is one index event. EventID is the idempotency key; delivery is a
// durable job, so the same event WILL arrive more than once.
type Event struct {
	EventID           string         `json:"event_id"`
	Kind              string         `json:"kind"`
	OccurredAt        time.Time      `json:"occurred_at"`
	SubjectType       string         `json:"subject_type"`
	SubjectID         string         `json:"subject_id"`
	VisibilityVersion *int64         `json:"visibility_version,omitempty"`
	Payload           map[string]any `json:"payload,omitempty"`
}

// EventBatch is a delivery. Batches are all-or-nothing.
type EventBatch struct {
	Site          Site    `json:"site"`
	Events        []Event `json:"events"`
	CorrelationID string  `json:"correlation_id,omitempty"`
}

// PublishResult acknowledges a batch.
type PublishResult struct {
	Status     Status `json:"status"`
	Accepted   int    `json:"accepted"`
	Duplicates int    `json:"duplicates"`
}

// Searcher is the frozen interface (ADR-002 § M0 obligations).
type Searcher interface {
	Search(ctx context.Context, req SearchRequest) (SearchResult, error)
	Suggest(ctx context.Context, req SuggestRequest) (SuggestResult, error)
	Publish(ctx context.Context, batch EventBatch) (PublishResult, error)
}

// Prober is implemented by backends that can report their own health.
type Prober interface {
	Probe(ctx context.Context) Health
}
