package stats

import (
	"testing"
	"time"
)

// проверяем nearest-rank на понятном наборе 1..10 мс, поданном вперемешку.
// Ранги: p50=ceil(.5*10)=5 -> 5мс, p95=ceil(9.5)=10 -> 10мс, p99=ceil(9.9)=10 -> 10мс.
func TestSummarizeNearestRank(t *testing.T) {
	lat := []time.Duration{
		5 * time.Millisecond, 1 * time.Millisecond, 9 * time.Millisecond,
		3 * time.Millisecond, 7 * time.Millisecond, 2 * time.Millisecond,
		10 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond,
		6 * time.Millisecond,
	}

	got := Summarize(lat)
	want := Latency{
		Min: 1 * time.Millisecond,
		P50: 5 * time.Millisecond,
		P95: 10 * time.Millisecond,
		P99: 10 * time.Millisecond,
		Max: 10 * time.Millisecond,
	}
	if got != want {
		t.Fatalf("Summarize = %+v, хотели %+v", got, want)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	if got := Summarize(nil); got != (Latency{}) {
		t.Fatalf("на пустом входе хотели нулевую сводку, получили %+v", got)
	}
}

func TestSummarizeSingle(t *testing.T) {
	got := Summarize([]time.Duration{42 * time.Millisecond})
	want := Latency{
		Min: 42 * time.Millisecond,
		P50: 42 * time.Millisecond,
		P95: 42 * time.Millisecond,
		P99: 42 * time.Millisecond,
		Max: 42 * time.Millisecond,
	}
	if got != want {
		t.Fatalf("на одном замере хотели %+v, получили %+v", want, got)
	}
}

// Summarize обязан быть чистой: сортировать копию, а не портить переданный срез.
func TestSummarizeDoesNotMutateInput(t *testing.T) {
	lat := []time.Duration{3 * time.Millisecond, 1 * time.Millisecond, 2 * time.Millisecond}
	before := append([]time.Duration(nil), lat...)

	Summarize(lat)

	for i := range lat {
		if lat[i] != before[i] {
			t.Fatalf("вход изменился: было %v, стало %v", before, lat)
		}
	}
}
