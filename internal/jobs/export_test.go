package jobs

// SafeErrorForTest exposes the unexported last_error sanitiser to the external
// test package. It exists so the redaction can be tested directly rather than
// only through a database round trip, and so the test cannot accidentally
// assert on a different code path than the one the worker uses.
func SafeErrorForTest(s string) string { return safeError(s) }
