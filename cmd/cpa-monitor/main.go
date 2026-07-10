package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mokexinxin/cpa-monitor/internal/app"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := app.Main(ctx, args, stdout, stderr)
	stop()
	return code
}
