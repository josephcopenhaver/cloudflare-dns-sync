package main

import (
	"context"

	"github.com/josephcopenhaver/cloudflare-dns-sync/internal/app"
)

func main() {
	ctx := context.Background()

	if err := app.Run(ctx); err != nil {
		panic(err)
	}
}
