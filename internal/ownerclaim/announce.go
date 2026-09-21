package ownerclaim

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// Announce modes. These mirror config.OwnerClaimAnnounce; the service takes a
// plain string so it does not import config.
const (
	AnnounceOff    = "off"
	AnnounceStderr = "stderr"
)

// BootOutcome is what Boot did, for the caller to log and for readiness.
type BootOutcome struct {
	Claimed    bool
	Minted     bool
	Superseded bool
	Generation int64
	// Degraded is set when the instance is unclaimed and boot could not
	// establish a usable state. The api marks readiness degraded rather than
	// refusing to start: an unreachable database or a schema behind 0005 is an
	// operator problem to diagnose, not a reason to crash-loop.
	Degraded bool
}

// Boot performs the first-run bootstrap and writes the operator-facing
// announcement to w.
//
// The announcement NEVER goes through the structured logger. internal/obs's
// redaction list includes the attribute key "token", so an honest key name would
// print "[redacted]" and tell the operator nothing, while any key name that
// survived redaction would ship a JSON field that a log shipper indexes and
// retains. So the announcement is plain text on a writer the caller chooses.
//
// The DEFAULT mode is off, and off prints a COMMAND rather than a credential.
// Every Docker log driver captures stdout and stderr of PID 1, ships it, retains
// it, and makes it searchable, and `docker compose logs` is the single most
// pasted artefact in issue trackers. A token there is a complete instance
// takeover for whoever reads it, so the safe default wins and the discoverable
// one is opt-in.
//
// Minting rule: boot mints ONLY when the operator has opted into stderr
// announcement AND no live token exists. A boot that finds a live token leaves
// it alone — which is what stops a compose `up -d`, an OOM, a health-check flap
// or an updater from silently invalidating the token in the operator's
// clipboard and leaving them with the deliberately uninformative "not accepted"
// message. `vizra claim-token` is the deliberate re-mint.
func Boot(ctx context.Context, pool *pgxpool.Pool, mode string, ttl time.Duration, w io.Writer) (BootOutcome, error) {
	q := sqlcgen.New(pool)

	claimed, err := q.AnyUserExists(ctx)
	if err != nil {
		return BootOutcome{Degraded: true}, fmt.Errorf("ownerclaim: reading claimed state: %w", err)
	}
	if claimed {
		// An implicitly claimed instance must never hold a live credential that
		// creates an owner, so a leftover token from before the first account is
		// retired here rather than left redeemable.
		n, err := SupersedeLive(ctx, pool)
		if err != nil {
			return BootOutcome{Claimed: true, Degraded: true}, err
		}
		return BootOutcome{Claimed: true, Superseded: n > 0}, nil
	}

	state, err := State(ctx, q)
	if err != nil {
		return BootOutcome{Degraded: true}, fmt.Errorf("ownerclaim: reading token state: %w", err)
	}

	if mode != AnnounceStderr {
		announceCommand(w, state)
		return BootOutcome{Generation: state.Generation}, nil
	}
	if state.Live {
		announceCommand(w, state)
		return BootOutcome{Generation: state.Generation}, nil
	}

	raw, generation, err := Mint(ctx, pool, ttl, false)
	if err != nil {
		return BootOutcome{Degraded: true}, err
	}
	fmt.Fprintf(w,
		"\nvizra: this instance is unclaimed.\n"+
			"  Owner claim token (generation %d), valid for %s — it works once:\n"+
			"    %s\n"+
			"  VIZRA_OWNER_CLAIM_ANNOUNCE=stderr wrote that credential to this log.\n"+
			"  If this instance's logs are shipped anywhere, set it to 'off' and use\n"+
			"    docker compose exec api vizra claim-token\n\n",
		generation, ttl, raw)
	return BootOutcome{Minted: true, Generation: generation}, nil
}

func announceCommand(w io.Writer, state TokenState) {
	fmt.Fprintf(w, "\nvizra: this instance is unclaimed. Get a claim token with:\n"+
		"    docker compose exec api vizra claim-token\n")
	if state.Live {
		fmt.Fprintf(w, "  (a token is already live: generation %d. Running the command above\n"+
			"   mints a new one and invalidates it.)\n", state.Generation)
	}
	fmt.Fprintln(w)
}
