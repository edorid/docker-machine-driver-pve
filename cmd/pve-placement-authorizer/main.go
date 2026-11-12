package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/edorid/docker-machine-driver-pve/internal/authorization"
)

func main() {
	os.Exit(run())
}

//nolint:wsl // Cancel the signal context before returning the process status.
func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := authorization.Run(ctx)
	cancel()

	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "placement authorization controller stopped")
		return 1
	}
	return 0
}
