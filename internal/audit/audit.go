// Package audit is the one way Vizra writes audit_events.
//
// It exists because migration 0003 froze a CHECK on ip_prefix and deferred its
// writer to "the first M1 writer" — this slice. Two rules follow from the
// schema and bind every later emitter:
//
//  1. The ip_prefix writer is TOTAL. Every audit insert shares the transaction
//     of the thing it records, so a value the frozen CHECK refuses does not
//     merely lose an audit row — it aborts the business transaction. For the
//     owner claim that would make an instance permanently unclaimable.
//
//  2. No PII beyond the username. audit_events_actor_user_fk is ON DELETE
//     RESTRICT (the frozen actor-identity CHECK makes SET NULL illegal), so a
//     user row naming an audit row can never be deleted and erasure is
//     scrub-and-tombstone. An email in `before`, `after` or `actor_label` would
//     therefore be undeletable. Usernames are public identifiers; emails are not.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"regexp"

	"github.com/google/uuid"

	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// Action names. Grammar: <domain>.<object>.<verb-past>, lower snake_case.
const (
	ActionOwnerClaimMinted      = "setup.owner_claim.minted"
	ActionOwnerClaimSuperseded  = "setup.owner_claim.superseded"
	ActionOwnerClaimRefused     = "setup.owner_claim.refused"
	ActionOwnerClaimRateLimited = "setup.owner_claim.rate_limited"
	ActionOwnerClaimSucceeded   = "setup.owner_claim.succeeded"
)

// Actor kinds, constrained by audit_events_actor_kind in migration 0003.
const (
	ActorSystem    = "system"
	ActorAnonymous = "anonymous"
	ActorUser      = "user"
	ActorAPIKey    = "api_key"
)

// Subject types.
const (
	SubjectOwnerClaimToken = "owner_claim_token"
	SubjectUser            = "user"
)

// Refusal reasons recorded on ActionOwnerClaimRefused. Deliberately coarse: the
// endpoint answers one message for every token failure, and an audit trail that
// distinguished them would reconstruct the oracle the response refuses to be.
const (
	ReasonTokenNotAccepted = "token_not_accepted"
	ReasonAlreadyClaimed   = "already_claimed"
)

// These are the ip_prefix grammar from migrations/0003_audit_events.up.sql,
// transcribed. TestIPPrefixGrammarMatchesTheMigration reads that file's bytes
// and asserts these are the same patterns, so the two cannot drift.
var (
	ipv4PrefixRe = regexp.MustCompile(`^((25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}0(/24)?$`)
	ipv6PrefixRe = regexp.MustCompile(`^[0-9a-f]{1,4}(:[0-9a-f]{1,4}){0,3}::(/(48|64))?$`)
)

// IPPrefix masks a remote address to the coarsest useful grouping and returns
// nil when there is no usable address. It implements the contract frozen in
// 0003's header: netip.ParseAddr then Unmap FIRST, /24 for IPv4 and /64 for
// IPv6 and never finer, lowercase, NULL when there is no usable address.
//
// It is total: no input of any shape can produce a string the CHECK refuses.
// That matters more than it looks. The frozen IPv6 pattern demands at least one
// hex group before "::", but masking ::1 (IPv6 loopback) or any address whose
// first 64 bits are zero yields exactly "::/64", and netip.Prefix.String()
// renders the zero Prefix as the literal text "invalid Prefix". Each of those
// violates the CHECK, and because the audit insert shares the claim's
// transaction, the 23514 would roll the claim back — the operator would be
// unable to claim the instance at all, forever, from that address. A caller
// dialling [::1]:8080 is not exotic; it is how a host-network test reaches it.
//
// Step 4 is therefore not belt-and-braces paranoia but the actual guarantee:
// whatever the masking produces is checked against the grammar before it can
// reach the database, and anything unexpected degrades to NULL, which 0003
// already prescribes as the honest answer for "no usable address".
func IPPrefix(remoteAddr string) *string {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	// A zone makes the address interface-scoped and ungroupable; 0003 names
	// zoned addresses as a NULL case.
	if addr.Zone() != "" {
		return nil
	}
	// Unmap FIRST, so ::ffff:203.0.113.5 is treated as the IPv4 address it is
	// and masked to /24 rather than /64.
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() {
		return nil
	}

	bits, re := 64, ipv6PrefixRe
	if addr.Is4() {
		bits, re = 24, ipv4PrefixRe
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return nil
	}
	s := prefix.String() // netip already lowercases
	if !re.MatchString(s) {
		return nil
	}
	return &s
}

// Valid reports whether s satisfies the frozen grammar. The handler calls this
// immediately before the insert, so no audit row can abort a business
// transaction even if IPPrefix is later changed carelessly.
func Valid(s string) bool {
	return ipv4PrefixRe.MatchString(s) || ipv6PrefixRe.MatchString(s)
}

// Event is one audit row. Zero values mean "absent".
type Event struct {
	ActorKind     string
	ActorUserID   *uuid.UUID
	ActorLabel    *string
	Action        string
	SubjectType   string
	SubjectID     *string
	Before        any
	After         any
	CorrelationID *string
	IPPrefix      *string
}

// Emit writes one audit row through q, which the caller has already bound to
// the transaction the audited change is happening in. It never starts its own
// transaction and never swallows an error: a claim that cannot be audited must
// not be committed.
func Emit(ctx context.Context, q *sqlcgen.Queries, ev Event) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("audit: generating id: %w", err)
	}
	before, err := marshal(ev.Before)
	if err != nil {
		return fmt.Errorf("audit: encoding before: %w", err)
	}
	after, err := marshal(ev.After)
	if err != nil {
		return fmt.Errorf("audit: encoding after: %w", err)
	}
	// Belt as well as braces (see IPPrefix): never let an out-of-grammar value
	// reach the INSERT and abort the caller's transaction.
	ip := ev.IPPrefix
	if ip != nil && !Valid(*ip) {
		ip = nil
	}
	_, err = q.InsertAuditEvent(ctx, sqlcgen.InsertAuditEventParams{
		ID:            id,
		ActorKind:     ev.ActorKind,
		ActorUserID:   ev.ActorUserID,
		ActorLabel:    ev.ActorLabel,
		Action:        ev.Action,
		SubjectType:   ev.SubjectType,
		SubjectID:     ev.SubjectID,
		Before:        before,
		After:         after,
		CorrelationID: ev.CorrelationID,
		IpPrefix:      ip,
	})
	if err != nil {
		return fmt.Errorf("audit: inserting %s: %w", ev.Action, err)
	}
	return nil
}

func marshal(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// String is a helper for the optional text fields.
func String(s string) *string { return &s }
