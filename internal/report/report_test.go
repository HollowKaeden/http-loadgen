package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/stats"
)

func sampleStats() stats.Stats {
	return stats.Stats{
		Total:     4,
		OK:        3,
		Failed:    1,
		ByStatus:  map[int]int{200: 3, 500: 1},
		ByError:   map[string]int{},
		Latencies: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond},
		Elapsed:   time.Second,
	}
}

// Build считает производные величины: RPS и перцентили в миллисекундах.
func TestBuildDerivedValues(t *testing.T) {
	r := Build(sampleStats())

	if r.Total != 4 || r.OK != 3 || r.Failed != 1 {
		t.Errorf("счётчики = %d/%d/%d, хотели 4/3/1", r.Total, r.OK, r.Failed)
	}
	if r.RPS != 4 {
		t.Errorf("RPS = %v, хотели 4 (4 запроса за 1с)", r.RPS)
	}
	if r.ElapsedMs != 1000 {
		t.Errorf("ElapsedMs = %v, хотели 1000", r.ElapsedMs)
	}
	// nearest-rank по 4 замерам: p50=ceil(.5*4)=2 -> 20мс, p95=ceil(3.8)=4 -> 40мс.
	if r.Latency.Min != 10 || r.Latency.P50 != 20 || r.Latency.P95 != 40 || r.Latency.Max != 40 {
		t.Errorf("Latency = %+v", r.Latency)
	}
}

// JSON это контракт для соседнего проекта: он должен быть детерминированным и держать
// пустую карту как {}, а не null, иначе схема на той стороне плывёт.
func TestWriteJSONStableAndEmptyMap(t *testing.T) {
	s := sampleStats()

	var a, b bytes.Buffer
	if err := WriteJSON(&a, s); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if err := WriteJSON(&b, s); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if a.String() != b.String() {
		t.Error("JSON не детерминирован: два прогона дали разный вывод")
	}

	out := a.String()
	if !strings.Contains(out, `"by_error": {}`) {
		t.Errorf("пустая карта ошибок должна быть {}, а не null:\n%s", out)
	}

	// вывод обязан быть валидным JSON с ожидаемыми ключами.
	var decoded map[string]any
	if err := json.Unmarshal(a.Bytes(), &decoded); err != nil {
		t.Fatalf("вывод не парсится как JSON: %v", err)
	}
	for _, key := range []string{"total", "ok", "failed", "elapsed_ms", "rps", "latency_ms", "by_status", "by_error"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("в JSON нет ключа %q", key)
		}
	}
}

func TestWriteText(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteText(&buf, sampleStats()); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"requests", "throughput", "latency p99", "status 200", "status 500"} {
		if !strings.Contains(out, want) {
			t.Errorf("в текстовом отчёте нет %q:\n%s", want, out)
		}
	}
}
