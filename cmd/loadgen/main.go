package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/HollowKaeden/http-loadgen/internal/config"
)

func main() {
	cfg, err := config.Parse(os.Args[1:], os.Stderr)
	if err != nil {
		// flag.ErrHelp значит, что usage уже напечатан по -h/-help, это не ошибка.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(2)
	}

	fmt.Printf("config OK: %+v\n", cfg)
}
