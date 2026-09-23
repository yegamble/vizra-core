package search

import (
	"context"
	"log/slog"

	"github.com/yegamble/vizra-core/internal/obs"
)

// Service is the composition callers hold. It owns the one rule Q-001 cares
// about: how a remote answer becomes either a clean fallback or a degraded
// signal.
//
//	SEARCH_MODE=off            -> sql, health off
//	remote answers not_indexed -> sql, health ok        (a decision, not a fault)
//	remote faults              -> sql, health degraded  (never silent)
//	remote answers ok          -> remote, health ok
type Service struct {
	sql    Searcher
	remote *RemoteClient
	log    *slog.Logger
}

// NewService builds the composition. remote may be nil, which is SEARCH_MODE=off.
func NewService(sqlImpl Searcher, remote *RemoteClient, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{sql: sqlImpl, remote: remote, log: log}
}

var _ Searcher = (*Service)(nil)

// Health is what /readyz reports for the search component.
func (s *Service) Health(ctx context.Context) Health {
	if s.remote == nil {
		return HealthOff
	}
	return s.remote.Probe(ctx)
}

// Enabled reports whether a remote service is configured at all.
func (s *Service) Enabled() bool { return s.remote != nil }

func (s *Service) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	if s.remote == nil {
		return s.sql.Search(ctx, req)
	}
	res, err := s.remote.Search(ctx, req)
	switch {
	case err != nil:
		// A configured but unreachable or misconfigured search is a degraded
		// readiness signal while requests are still served from SQL — never a
		// silent fallback (Q-001).
		//
		// obs.Redact at the call site, not left to the handler: NewService falls
		// back to slog.Default(), and a VIZRA_SEARCH_URL that fails to parse
		// comes back from http.NewRequestWithContext quoted whole, userinfo and
		// query included (B3 verifier, core #12 V-1;
		// TestTheFallbackLogLinesAreRedacted).
		s.log.Warn("search: falling back to SQL after a remote fault", "error", obs.Redact(err.Error()))
		return s.sql.Search(ctx, req)
	case res.Status == StatusNotIndexed:
		// Not a fault. The service is healthy and is telling us it holds no index.
		return s.sql.Search(ctx, req)
	default:
		return res, nil
	}
}

func (s *Service) Suggest(ctx context.Context, req SuggestRequest) (SuggestResult, error) {
	if s.remote == nil {
		return s.sql.Suggest(ctx, req)
	}
	res, err := s.remote.Suggest(ctx, req)
	switch {
	case err != nil:
		s.log.Warn("search: falling back to SQL after a remote fault", "error", obs.Redact(err.Error()))
		return s.sql.Suggest(ctx, req)
	case res.Status == StatusNotIndexed:
		return s.sql.Suggest(ctx, req)
	default:
		return res, nil
	}
}

// Publish delivers index events. Unlike the read paths it returns the error,
// because publication is driven by a durable job (ADR-004): the job must fail
// and be retried along the ladder, not be silently swallowed the way Vidra's
// best-effort search outbox was.
//
// `not_indexed` is the exception: the service deliberately discarded the batch,
// so the job succeeded and must not be retried forever.
func (s *Service) Publish(ctx context.Context, batch EventBatch) (PublishResult, error) {
	if s.remote == nil {
		return s.sql.Publish(ctx, batch)
	}
	res, err := s.remote.Publish(ctx, batch)
	if err != nil {
		return PublishResult{}, err
	}
	return res, nil
}
