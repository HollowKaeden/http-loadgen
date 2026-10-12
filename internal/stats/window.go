package stats

import "time"

// Window это метрики за один интервал прогона (обычно секунду). Нужны тому, кто
// смотрит на прогон вживую: веб-форма рисует по ним график, а аварийный тормоз
// решает, не пора ли остановиться. CLI окна не использует, ему хватает итога.
type Window struct {
	Index   int           // номер интервала с нуля
	Total   int           // запросов, завершившихся в этом интервале
	OK      int           // из них 2xx
	Failed  int           // не-2xx плюс транспортные ошибки
	Elapsed time.Duration // длина интервала; последний бывает короче
	Latency Latency       // сводка латентности только по запросам интервала
}

// RPS это скорость в интервале: сколько запросов завершилось за секунду.
func (w Window) RPS() float64 {
	if w.Elapsed <= 0 {
		return 0
	}
	return float64(w.Total) / w.Elapsed.Seconds()
}

// ErrorRate это доля неуспешных запросов в интервале, от 0 до 1.
func (w Window) ErrorRate() float64 {
	if w.Total == 0 {
		return 0
	}
	return float64(w.Failed) / float64(w.Total)
}

// CollectLive делает то же, что Collect, и вдобавок раз в interval отдаёт в onWindow
// метрики за прошедший интервал. Последний, неполный интервал отдаётся при закрытии
// канала, если в нём что-то было, поэтому сумма Total по окнам равна итоговому Total.
//
// onWindow вызывается из той же горутины-коллектора, что копит итог: так окна и итог
// считаются без синхронизации. Цена этого в том, что колбэк должен быть быстрым и не
// блокироваться, иначе встанет чтение results, а за ним и воркеры.
func CollectLive(results <-chan Result, interval time.Duration, onWindow func(Window)) Stats {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	return collectLive(results, ticker.C, time.Now, onWindow)
}

// collectLive это CollectLive с подменяемыми часами: тесты подают тики через свой
// канал и сами решают, когда кончился интервал, без time.Sleep.
func collectLive(results <-chan Result, ticks <-chan time.Time, now func() time.Time, onWindow func(Window)) Stats {
	s := Stats{
		ByStatus: make(map[int]int),
		ByError:  make(map[string]int),
	}

	var (
		cur       Window
		latencies []time.Duration
		started   = now()
	)

	// flush закрывает текущий интервал моментом end: досчитывает сводку, отдаёт окно
	// и начинает новое с того же момента.
	flush := func(end time.Time) {
		cur.Elapsed = end.Sub(started)
		cur.Latency = Summarize(latencies)
		onWindow(cur)
		cur = Window{Index: cur.Index + 1}
		latencies = latencies[:0]
		started = end
	}

	for {
		select {
		case res, ok := <-results:
			if !ok {
				if cur.Total > 0 {
					flush(now())
				}
				return s
			}
			s.add(res)
			cur.Total++
			latencies = append(latencies, res.Latency)
			if res.Err == nil && res.Status >= 200 && res.Status < 300 {
				cur.OK++
			} else {
				cur.Failed++
			}
		case tick := <-ticks:
			// Границу берём из самого тика, а не из now(): если коллектор замешкался
			// с обработкой, окно всё равно закроется ровно по расписанию.
			// Пустые интервалы тоже отдаём: на графике это честный ноль, а тормоз
			// видит, что сервер перестал отвечать.
			flush(tick)
		}
	}
}
