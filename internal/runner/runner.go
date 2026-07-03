package runner

import (
	"context"
	"fmt"

	"github.com/HollowKaeden/http-loadgen/internal/config"
)

func Run(ctx context.Context, cfg *config.Config) error {
	client := newClient(cfg)
	res := doRequest(ctx, client, cfg)

	if res.Err != nil {
		fmt.Printf("запрос не дошёл: %v (ждали %s)\n", res.Err, res.Latency)
		return nil
	}
	fmt.Printf("статус %d, латентность %s\n", res.Status, res.Latency)
	return nil
}
