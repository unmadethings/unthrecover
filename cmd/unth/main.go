// Command unth is the Unth Recover CLI.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // offsite.time_zone works on minimal images without zoneinfo

	"github.com/unmadethings/unthrecover/internal/cli"
)

func main() {
	// SIGINT/SIGTERM cancel the context; engines then clean up their
	// isolated resources and record the operation as CANCELLED.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
