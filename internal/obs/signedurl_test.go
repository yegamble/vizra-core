package obs

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestDenylistSignedURLs asserts explicitly (VZ-FOUND-006 privacy case) that
// a private signed URL never reaches the real logger's output, whatever key,
// group or position carries it: the signature, credential and session-token
// query values of AWS SigV4 presigned, GCS V4 and legacy/CloudFront-style
// signed URLs are all redacted.
func TestDenylistSignedURLs(t *testing.T) {
	secrets := []string{"awssig7Q", "awscred7Q", "awstok7Q", "goosig7Q", "goocred7Q", "cfsig7Q"}
	urls := []string{
		"https://bucket.s3.us-east-1.amazonaws.com/private/o.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=awscred7Q&X-Amz-Date=20261112T000000Z&X-Amz-Expires=300&X-Amz-Security-Token=awstok7Q&X-Amz-SignedHeaders=host&X-Amz-Signature=awssig7Q",
		"https://storage.googleapis.com/b/o.jpg?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential=goocred7Q&X-Goog-Expires=300&X-Goog-Signature=goosig7Q",
		"https://d111.cloudfront.net/o.jpg?Expires=1&Signature=cfsig7Q&Key-Pair-Id=K1",
	}
	for _, u := range urls {
		var buf bytes.Buffer
		l := NewLogger(&buf, true)
		l.Info("fetch", "url", u)
		l.Info("fetch", slog.Group("req", slog.String("location", u)))
		l.WithGroup("g").With("redirect", u).Info("with")
		l.Info("fetching " + u)
		l.Error("fetch", "error", fmt.Errorf("GET %s: %w", u, errors.New("403")))
		assertJSONLines(t, buf.String(), 5)
		for _, s := range secrets {
			if strings.Contains(buf.String(), s) {
				t.Errorf("signed-URL value %q leaked: %s", s, buf.String())
			}
		}
	}
}
