// Command autobot runs recurring chores with an AI model.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jgabor/autobot/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
