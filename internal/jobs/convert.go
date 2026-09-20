package jobs

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func toTimestamptz(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func fromTimestamptz(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// toInterval converts a Go duration to the pgtype.Interval sqlc infers for an
// `::interval` cast parameter. Microseconds are exact for every duration the
// lease and retry ladder use.
func toInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
