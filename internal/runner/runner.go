package runner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/stats"
)

// Run поднимает пул из cfg.Concurrency воркеров и раздаёт им работу через канал
// jobs. Воркеры шлют исходы в results, откуда мы их пока считаем прямо здесь.
func Run(ctx context.Context, cfg *config.Config) error {
	client := newClient(cfg)

	jobs := make(chan struct{})
	results := make(chan stats.Result, cfg.Concurrency)

	go produce(ctx, cfg, jobs)

	var wg sync.WaitGroup
	wg.Add(cfg.Concurrency)
	for i := 0; i < cfg.Concurrency; i++ {
		go func() {
			defer wg.Done()
			worker(ctx, client, cfg, jobs, results)
		}()
	}

	// results закрывает тот, кто в него пишет. Пишут воркеры, но каждый по
	// отдельности не знает, последний ли он, поэтому закрытие отдаём отдельной
	// горутине: она ждёт всех через WaitGroup и закрывает канал, чтобы цикл ниже
	// корректно завершился.
	go func() {
		wg.Wait()
		close(results)
	}()

	var total, ok, failed int
	for res := range results {
		total++
		if res.Err == nil && res.Status >= 200 && res.Status < 300 {
			ok++
		} else {
			failed++
		}
	}

	fmt.Printf("total=%d ok=%d failed=%d\n", total, ok, failed)
	return nil
}

// produce раздаёт задания воркерам и закрывает jobs, когда работа кончилась.
// Форма остановки зависит от режима:
//
//	-n: раздать ровно cfg.Requests заданий.
//	-d: раздавать, пока не истечёт cfg.Duration.
func produce(ctx context.Context, cfg *config.Config, jobs chan<- struct{}) {
	defer close(jobs)

	if cfg.DurationSet {
		deadline := time.After(cfg.Duration)
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline:
				return
			case jobs <- struct{}{}:
			}
		}
	}

	for i := 0; i < cfg.Requests; i++ {
		select {
		case <-ctx.Done():
			return
		case jobs <- struct{}{}:
		}
	}
}
