/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command sc-snapshot fetches a fixed set of Spare Cores Keeper API responses
// into a directory that package keepertest replays as a fake Keeper:
//
//	sc-snapshot -queries queries.yaml -out testdata/snapshot
//
// The query list (see package snapshot) names the requests; the output
// directory is replaced as a whole; -resume skips the queries an earlier run
// already wrote. KEEPER_URL is read from the environment. The API key, which
// /table/server_prices and the snapshot routes (a "snapshot:" block in the
// query list) need, comes from the file named by -api-key-file or else from
// KEEPER_API_KEY. It is only sent to Keeper: the output has no request
// headers.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/SpareCores/sc-keeper-go/snapshot"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sc-snapshot:", err)
		os.Exit(1)
	}
}

func run() error {
	queries := flag.String("queries", "", "query list file (YAML)")
	out := flag.String("out", "", "output directory (replaced)")
	keyFile := flag.String("api-key-file", "", "file with the Keeper API key (default: the KEEPER_API_KEY environment variable)")
	resume := flag.Bool("resume", false, "skip queries whose output already exists, is complete and has the same Keeper database hash")
	flag.Parse()
	if *queries == "" || *out == "" {
		flag.Usage()
		return fmt.Errorf("-queries and -out are required")
	}
	list, err := snapshot.LoadQueryList(*queries)
	if err != nil {
		return err
	}
	key := os.Getenv("KEEPER_API_KEY")
	if *keyFile != "" {
		raw, err := os.ReadFile(*keyFile)
		if err != nil {
			return fmt.Errorf("reading the API key file: %w", err)
		}
		if key = strings.TrimSpace(string(raw)); key == "" {
			return fmt.Errorf("the API key file %s is empty", *keyFile)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	snap, err := snapshot.Fetch(ctx, list, snapshot.Options{
		Dir:       *out,
		Resume:    *resume,
		Log:       func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
		KeeperURL: os.Getenv("KEEPER_URL"),
		APIKey:    key,
	})
	if err != nil {
		return err
	}
	m := snap.Manifest
	fmt.Fprintf(os.Stderr, "snapshot of Keeper database %s (%s): %d queries (%d reused) -> %s\n",
		m.Keeper.DatabaseHash, m.Keeper.DatabaseLastUpdated, len(m.Queries), snap.Reused, *out)
	return nil
}
