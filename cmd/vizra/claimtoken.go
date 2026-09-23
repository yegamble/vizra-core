package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/db"
	"github.com/yegamble/vizra-core/internal/ownerclaim"
	"github.com/yegamble/vizra-core/internal/site"
)

// runClaimToken mints a fresh owner-claim token and prints it to STDOUT.
//
// Stdout is the whole point. Run as `docker compose exec api vizra claim-token`,
// this output goes to the operator's terminal and is NOT part of the api
// container's captured log stream — so it is the one delivery mechanism whose
// output no log driver ships, no aggregator indexes and no retention policy
// keeps. VZ-INSTALL-003's privacy case ("Token never appears in HTTP responses
// or non-local logs") is why this, and not a boot line, is the primary path.
//
// It ALWAYS supersedes the previous token and mints a new one: re-minting is an
// explicit operator action with a predictable result, which is exactly what
// makes it a better recovery path than "restart the process and hope".
func runClaimToken(args []string) error {
	fs := flag.NewFlagSet("claim-token", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("vizra claim-token refused to run.\n%w", err)
	}
	ctx := context.Background()
	resolver := site.NewResolver(cfg)

	pools, err := db.Open(ctx, resolver)
	if err != nil {
		return fmt.Errorf("vizra claim-token could not reach the database: %w", err)
	}
	defer pools.Close()

	raw, generation, err := // refuseIfUsersExist=true, onlyIfNoLiveToken=false: a deliberate re-mint
		// must ALWAYS supersede, which is what makes this a usable recovery path.
		ownerclaim.Mint(ctx, pools.Default(), cfg.OwnerClaimTTL, true, false)
	if errors.Is(err, ownerclaim.ErrHasUsers) {
		// Refusing here is a security decision, not tidiness. Minting on a
		// claimed instance would manufacture a live owner-creating credential on
		// a running production system — a standing escalation path for anyone
		// who can reach `docker exec` or the DSN, and a landmine for any future
		// owner-transfer route.
		return errors.New(
			"vizra claim-token: this instance already has accounts, so it is already claimed.\n" +
				"No token was minted. Owner claim runs exactly once in the life of an instance;\n" +
				"if you have lost access to the owner account, that is an account-recovery\n" +
				"problem, not a re-claim.")
	}
	if err != nil {
		return fmt.Errorf("vizra claim-token could not mint a token: %w", err)
	}

	// The token itself goes to stdout; everything explanatory goes to stderr, so
	// `vizra claim-token | pbcopy` and similar pipes carry the token alone.
	fmt.Fprintf(os.Stderr, "Owner claim token (generation %d), valid for %s.\n"+
		"It works once. Paste it into the claim page at %s/setup/claim\n"+
		"Running this command again mints a new token and invalidates this one.\n\n",
		generation, cfg.OwnerClaimTTL, cfg.PublicOrigin)
	fmt.Println(raw)
	return nil
}
