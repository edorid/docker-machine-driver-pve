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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := authorization.Run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "placement authorization controller stopped")
		os.Exit(1)
	}
}
