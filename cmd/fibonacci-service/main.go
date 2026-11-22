// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main implements the entrypoint of the Fibonacci service.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aozmekik/fibonacci-service/internal/app"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The first signal starts the shutdown. Stop listening for signals at that point,
	// so that a second one terminates the process without waiting for it.
	context.AfterFunc(ctx, stop)

	return app.Run(ctx, os.Args[1:])
}
