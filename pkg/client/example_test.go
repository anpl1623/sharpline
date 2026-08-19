package client_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/anpl1623/sharpline/pkg/client"
)

// Example is a complete program: read today's board without authenticating,
// then open a session and read the derived play-money balance.
//
// It reads the password from the environment rather than taking it as a flag or
// a literal, because a password on a command line is in the process table and
// in the shell history.
func Example() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := client.New(client.Options{
		BaseURL: "https://sharpline.example",
	})
	if err != nil {
		log.Fatal(err)
	}

	// ---- public: no credential needed --------------------------------------
	limit := int32(20)
	format := client.OddsFormatAmerican
	board, err := c.Board(ctx, client.GetBoardParams{
		Limit:      &limit,
		OddsFormat: &format,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range board.Data {
		// A futures market has no two competitors, so both are optional and
		// the event's own Name is the field that is always populated.
		fmt.Printf("%s (%s, %d markets)\n", entry.Event.Name, entry.Event.Status, len(entry.Markets))
	}
	// Staleness is measured against the page's own instant, never the local
	// clock: a skewed client clock would otherwise make a fresh board look old.
	fmt.Printf("board assembled %s ago\n", time.Since(board.AsOf).Truncate(time.Second))

	// ---- authenticated ------------------------------------------------------
	creds := client.Credentials{
		Email:    os.Getenv("SHARPLINE_EMAIL"),
		Password: os.Getenv("SHARPLINE_PASSWORD"),
	}

	sess, err := c.Login(ctx, creds)
	if errors.Is(err, client.ErrTOTPRequired) {
		// The account has a confirmed second factor. Prompt, then retry — do
		// not ask every user for a code up front, which would reveal which
		// accounts have 2FA enabled.
		creds.TOTPCode = os.Getenv("SHARPLINE_TOTP_CODE")
		sess, err = c.Login(ctx, creds)
	}
	if err != nil {
		log.Fatal(err)
	}

	// The refresh token rotates on every use. Persist the new one HERE, not by
	// polling: a stored copy one rotation behind looks like reuse when it is
	// presented, and reuse revokes the whole login family.
	sess.OnRotate(func(t client.Tokens) {
		// t.String() and t.LogValue() both redact, so this cannot leak by
		// accident. Writing t.RefreshToken to a keychain is an explicit act.
		_ = t
	})

	auth := c.WithSession(sess)

	balance, err := auth.Balance(ctx)
	if err != nil {
		log.Fatal(err)
	}
	// Integer minor units all the way to the screen; FormatMinor does the
	// division without touching a float.
	fmt.Printf("cash %s %s across %d ledger entries\n",
		client.FormatMinor(balance.Cash.BalanceMinor),
		balance.Currency,
		balance.Cash.EntryCount,
	)

	if err := sess.Logout(ctx); err != nil {
		log.Print(err)
	}
}

// ExampleClient_Board_paging walks every page of the board with a keyset
// cursor.
//
// Pagination is keyset rather than offset because ingest writes continuously:
// with an offset, a row inserted ahead of the cursor between two pages pushes
// another row across the boundary and the reader never sees it. Pass the cursor
// back unchanged and change nothing else about the query — a cursor is bound to
// the ordering and filters it was minted under.
func ExampleClient_Board_paging() {
	ctx := context.Background()
	c, err := client.New(client.Options{BaseURL: "https://sharpline.example"})
	if err != nil {
		log.Fatal(err)
	}

	params := client.GetBoardParams{}
	for {
		page, err := c.Board(ctx, params)
		if err != nil {
			log.Fatal(err)
		}
		for _, entry := range page.Data {
			fmt.Println(entry.Event.Id)
		}
		if !page.Page.HasMore || page.Page.NextCursor == nil {
			break
		}
		params.Cursor = page.Page.NextCursor
	}
}

// ExampleClient_SetLimit shows the asymmetry that makes a self-imposed limit a
// real control: tightening binds immediately, loosening serves a cooling-off
// period. Always read the returned EffectiveFrom rather than assuming the limit
// is in force.
func ExampleClient_SetLimit() {
	ctx := context.Background()
	c, err := client.New(client.Options{BaseURL: "https://sharpline.example"})
	if err != nil {
		log.Fatal(err)
	}
	auth := c.WithSession(c.Resume(os.Getenv("SHARPLINE_REFRESH_TOKEN")))

	amount := client.MoneyMinor(50_000) // 500.00 in minor units
	limit, err := auth.SetLimit(ctx, client.SetLimitRequest{
		Kind:        client.LimitKindLoss,
		Period:      client.LimitPeriodWeek,
		AmountMinor: &amount,
	})
	if err != nil {
		log.Fatal(err)
	}

	if limit.InForce {
		fmt.Println("in force now")
	} else {
		fmt.Printf("takes effect %s\n", limit.EffectiveFrom.Format(time.RFC3339))
	}
}
