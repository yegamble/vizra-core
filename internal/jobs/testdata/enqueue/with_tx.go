// POSITIVE CONTROL for TestEnqueueRequiresATransaction.
//
// This program passes a pgx.Tx and MUST compile. If it stops compiling the test
// harness itself is broken, and the negative case below would "pass" for the
// wrong reason.
package main

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/yegamble/vizra-core/internal/jobs"
)

func main() {
	var tx pgx.Tx
	_, _ = jobs.Enqueue(context.Background(), tx, jobs.NewJob{
		Kind:          jobs.KindNoop,
		CorrelationID: "c1",
	})
}
