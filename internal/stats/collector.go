package stats

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

// Stats это агрегированный итог прогона. Заполняется одной горутиной-коллектором,
// поэтому счётчики и карты живут без мьютексов.
type Stats struct {
	Total     int             // всего сделанных запросов
	OK        int             // ответы 2xx
	Failed    int             // не-2xx плюс транспортные ошибки
	ByStatus  map[int]int     // гистограмма HTTP-кодов
	ByError   map[string]int  // гистограмма типов ошибок (timeout, connection refused, ...)
	Latencies []time.Duration // все замеры латентности, для перцентилей
	Elapsed   time.Duration   // реальное время прогона, для RPS
}

// Collect читает исходы из results до закрытия канала и сворачивает их в Stats.
// Это единственный потребитель канала, поэтому карты и счётчики трогает только он,
// и синхронизация не нужна: конкурентного доступа к Stats нет by design.
func Collect(results <-chan Result) Stats {
	s := Stats{
		ByStatus: make(map[int]int),
		ByError:  make(map[string]int),
	}
	for res := range results {
		s.add(res)
	}
	return s
}

// add учитывает один исход. Латентность копим всегда, даже у ошибок: время до
// таймаута или обрыва тоже часть картины.
func (s *Stats) add(res Result) {
	s.Total++
	s.Latencies = append(s.Latencies, res.Latency)

	if res.Err != nil {
		s.Failed++
		s.ByError[classifyError(res.Err)]++
		return
	}

	s.ByStatus[res.Status]++
	if res.Status >= 200 && res.Status < 300 {
		s.OK++
	} else {
		s.Failed++
	}
}

// classifyError сводит конкретную ошибку к короткой категории, чтобы гистограмма
// не дробилась на уникальные строки (в них попадает URL и адрес).
func classifyError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "refused"):
		return "connection refused"
	case strings.Contains(msg, "reset"):
		return "connection reset"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dns"):
		return "dns error"
	case strings.Contains(msg, "eof"):
		return "eof"
	default:
		return "other"
	}
}
