package stats

import (
	"math"
	"sort"
	"time"
)

// Latency это сводка по латентности за прогон: крайние значения и три перцентиля.
// Отдельная структура, чтобы отчёт печатал одно готовое значение, а не пересчитывал.
type Latency struct {
	Min time.Duration
	Max time.Duration
	P50 time.Duration
	P95 time.Duration
	P99 time.Duration
}

// Summarize считает сводку по замерам латентности. Функция чистая: сортирует копию,
// а не сам срез, поэтому не портит порядок Latencies у коллектора и удобна в тестах.
// На пустом входе возвращает нулевую сводку.
func Summarize(latencies []time.Duration) Latency {
	if len(latencies) == 0 {
		return Latency{}
	}

	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	return Latency{
		Min: sorted[0],
		Max: sorted[len(sorted)-1],
		P50: percentile(sorted, 50),
		P95: percentile(sorted, 95),
		P99: percentile(sorted, 99),
	}
}

// percentile берёт p-й перцентиль из уже отсортированного среза методом nearest-rank:
// ранг это ceil(p/100 * N), берём значение под этим рангом (нумерация с единицы).
// Никакой интерполяции между соседями: возвращаем реально измеренную величину, что
// честнее для латентности и не выдумывает несуществующих точек. Вход считаем непустым.
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
