package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/WyattJia/youtube-playlist-cli/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(os.Args[1:], cli.Options{Context: ctx})
	cancel()
	os.Exit(code)
}
