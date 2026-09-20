package search

import "context"

// SQLSearcher is the permanent PostgreSQL FTS/trigram implementation of the
// boundary (Q-001: "a permanent `sql` implementation ... and a `remote`
// client"). It is not a stub that will be deleted: it is the path every request
// falls back to when search is off, not indexed, or faulted, for the life of
// the project.
//
// In M0 it returns empty results because no product tables exist yet. The
// honest signal for that is StatusOK with zero results — the index it queries
// is genuinely empty — and NOT StatusNotIndexed, which belongs to the remote
// service saying it holds no index.
type SQLSearcher struct{}

// NewSQL builds the SQL searcher. It takes no database handle yet because it
// has no tables to read; VZ-SEARCH-001 (M3) gives it one, resolved from the
// request context like every other handle (Q-008 checklist item 1), never from
// a package global.
func NewSQL() *SQLSearcher { return &SQLSearcher{} }

var _ Searcher = (*SQLSearcher)(nil)

func (s *SQLSearcher) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return SearchResult{}, err
	}
	return SearchResult{Status: StatusOK, Results: []Hit{}, Total: 0}, nil
}

func (s *SQLSearcher) Suggest(ctx context.Context, req SuggestRequest) (SuggestResult, error) {
	if err := ctx.Err(); err != nil {
		return SuggestResult{}, err
	}
	return SuggestResult{Status: StatusOK, Suggestions: []Suggestion{}}, nil
}

// Publish is a no-op for the SQL path: there is no separate index to write to,
// because the SQL implementation reads the same tables the mutation wrote. It
// reports the events as accepted so a caller cannot distinguish "published" from
// "dropped" by the return value and then retry forever.
func (s *SQLSearcher) Publish(ctx context.Context, batch EventBatch) (PublishResult, error) {
	if err := ctx.Err(); err != nil {
		return PublishResult{}, err
	}
	return PublishResult{Status: StatusOK, Accepted: len(batch.Events)}, nil
}

func (s *SQLSearcher) Probe(ctx context.Context) Health { return HealthOK }
