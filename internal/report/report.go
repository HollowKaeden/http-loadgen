// Package report превращает собранную статистику в вывод: человекочитаемую таблицу
// для глаз и стабильный JSON для машин. JSON это контракт: по нему соседний проект
// строит сравнение реализаций, поэтому набор и имена полей менять с осторожностью.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/stats"
)

// Report это машиночитаемая форма итога. Времена отдаём в миллисекундах числом,
// а не строкой-длительностью: так потребителю не нужно парсить "10.13ms", можно
// сразу класть в таблицу и сравнивать. Имена полей snake_case и фиксированы.
type Report struct {
	Total     int            `json:"total"`
	OK        int            `json:"ok"`
	Failed    int            `json:"failed"`
	ElapsedMs float64        `json:"elapsed_ms"`
	RPS       float64        `json:"rps"`
	Latency   latencyReport  `json:"latency_ms"`
	ByStatus  map[int]int    `json:"by_status"`
	ByError   map[string]int `json:"by_error"`
}

// latencyReport это перцентили латентности в миллисекундах.
type latencyReport struct {
	Min float64 `json:"min"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// Build сворачивает Stats в Report. Единственное место, где считаются производные
// величины (RPS, перцентили, перевод в мс), чтобы текст и JSON показывали одни и те
// же числа, а не разошлись из-за двух независимых расчётов.
func Build(s stats.Stats) Report {
	lat := stats.Summarize(s.Latencies)

	var rps float64
	if s.Elapsed > 0 {
		rps = float64(s.Total) / s.Elapsed.Seconds()
	}

	return Report{
		Total:     s.Total,
		OK:        s.OK,
		Failed:    s.Failed,
		ElapsedMs: round2(ms(s.Elapsed)),
		RPS:       round2(rps),
		Latency: latencyReport{
			Min: round2(ms(lat.Min)),
			P50: round2(ms(lat.P50)),
			P95: round2(ms(lat.P95)),
			P99: round2(ms(lat.P99)),
			Max: round2(ms(lat.Max)),
		},
		ByStatus: s.ByStatus,
		ByError:  s.ByError,
	}
}

// Write печатает отчёт в выбранном формате. Одна точка входа, чтобы main не знал,
// какой рендер вызывать, а просто передал флаг.
func Write(w io.Writer, s stats.Stats, asJSON bool) error {
	if asJSON {
		return WriteJSON(w, s)
	}
	return WriteText(w, s)
}

// WriteJSON печатает стабильный машиночитаемый отчёт. Отступы для читаемости и diff'ов,
// ключи карт encoding/json сортирует сам, так что вывод детерминирован при равных данных.
func WriteJSON(w io.Writer, s stats.Stats) error {
	data, err := json.MarshalIndent(Build(s), "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

// WriteText печатает человекочитаемую сводку. Времена показываем как Duration
// (10.13ms), это привычнее глазу, чем голые миллисекунды из JSON.
func WriteText(w io.Writer, s stats.Stats) error {
	lat := stats.Summarize(s.Latencies)

	var rps float64
	if s.Elapsed > 0 {
		rps = float64(s.Total) / s.Elapsed.Seconds()
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintf(tw, "requests\t%d total, %d ok, %d failed\n", s.Total, s.OK, s.Failed)
	fmt.Fprintf(tw, "duration\t%s\n", s.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(tw, "throughput\t%.1f req/s\n", rps)
	fmt.Fprintf(tw, "\t\n")
	fmt.Fprintf(tw, "latency min\t%s\n", dur(lat.Min))
	fmt.Fprintf(tw, "latency p50\t%s\n", dur(lat.P50))
	fmt.Fprintf(tw, "latency p95\t%s\n", dur(lat.P95))
	fmt.Fprintf(tw, "latency p99\t%s\n", dur(lat.P99))
	fmt.Fprintf(tw, "latency max\t%s\n", dur(lat.Max))

	if len(s.ByStatus) > 0 {
		fmt.Fprintf(tw, "\t\n")
		for _, code := range sortedIntKeys(s.ByStatus) {
			fmt.Fprintf(tw, "status %d\t%d\n", code, s.ByStatus[code])
		}
	}

	if len(s.ByError) > 0 {
		fmt.Fprintf(tw, "\t\n")
		for _, kind := range sortedStringKeys(s.ByError) {
			fmt.Fprintf(tw, "error %s\t%d\n", kind, s.ByError[kind])
		}
	}

	return tw.Flush()
}

// ms переводит длительность в миллисекунды с плавающей точкой.
func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// round2 округляет до двух знаков после запятой: микросекундной точности в отчёте
// достаточно, а лишний хвост только зашумляет числа.
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

// dur укорачивает длительность до микросекунд для печати: наносекундный хвост в
// таблице не несёт смысла на сетевых временах.
func dur(d time.Duration) time.Duration {
	return d.Round(time.Microsecond)
}

func sortedIntKeys(m map[int]int) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func sortedStringKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
