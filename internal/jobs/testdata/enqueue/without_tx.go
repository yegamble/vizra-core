// NEGATIVE CASE for TestEnqueueRequiresATransaction.
//
// This program passes a *pgxpool.Pool, which is what a caller has when they are
// NOT inside a transaction. ADR-004 requires that a job row is written in the
// same transaction as the business mutation, and this file is the proof that
// the rule is enforced by the compiler: it MUST NOT compile.
package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/jobs"
)

func main() {
	var pool *pgxpool.Pool
	_, _ = jobs.Enqueue(context.Background(), pool, jobs.NewJob{
		Kind:          jobs.KindNoop,
		CorrelationID: "c1",
	})
}
