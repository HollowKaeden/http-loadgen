package runner

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/stats"
)

// newClient собирает единственный http.Client на весь прогон. Все воркеры делят
// один клиент, поэтому Transport надо настроить с запасом idle-коннектов на хост.
// Иначе на каждый запрос пересоздавалось бы TCP-соединение, и мы мерили бы не
// сервис, а стоимость установки коннекта. MaxIdleConnsPerHost держим на уровне
// числа воркеров, чтобы всем хватило горячих соединений.
func newClient(cfg *config.Config) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = cfg.Concurrency
	t.MaxIdleConnsPerHost = cfg.Concurrency

	return &http.Client{
		Timeout:   cfg.Timeout,
		Transport: t,
	}
}

// doRequest выполняет ровно один запрос и возвращает Result. Паники тут нет: любая
// ошибка (сборка запроса или транспорт) превращается в Result с заполненным Err.
func doRequest(ctx context.Context, client *http.Client, cfg *config.Config) stats.Result {
	var body io.Reader
	if cfg.Body != "" {
		body = strings.NewReader(cfg.Body)
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, body)
	if err != nil {
		return stats.Result{Err: err}
	}
	for key, vals := range cfg.Headers {
		for _, v := range vals {
			req.Header.Add(key, v)
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// Латентность до ошибки тоже осмысленна: таймаут покажет, сколько ждали.
		return stats.Result{Latency: time.Since(start), Err: err}
	}
	defer resp.Body.Close()

	// Дочитываем тело до конца и закрываем. Без этого keep-alive соединение не
	// вернётся в пул, следующий запрос откроет новое, и весь смысл общего клиента
	// пропадёт. Это классический Go-факап, про который любят спросить на собесе.
	_, _ = io.Copy(io.Discard, resp.Body)

	return stats.Result{Latency: time.Since(start), Status: resp.StatusCode}
}
