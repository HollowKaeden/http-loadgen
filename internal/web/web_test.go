package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock это подменяемые часы планировщика. Под мьютексом: их читает и горутина
// завершения прогона, и сам тест.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testSettings() Settings {
	return Settings{
		Cooldown:       3 * time.Minute,
		MaxDuration:    15 * time.Second,
		MinShare:       10,
		MaxShare:       50,
		MaxWorkers:     16,
		RequestTimeout: 2 * time.Second,
		IPLimit:        2,
		IPWindow:       15 * time.Minute,
		AbandonAfter:   time.Minute, // в обычных тестах прогон не должен отменяться сам
		Brake:          Brake{Windows: 3, MaxErrorRate: 0.05, MaxP95: time.Second, MinRequests: 5},
	}
}

// newTestScheduler поднимает цель на httptest и планировщик с подменёнными часами.
func newTestScheduler(t *testing.T, handler http.HandlerFunc, settings Settings) (*Scheduler, *clock) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	targets := &Targets{Targets: []Target{
		{ID: "ok", Title: "Цель", URL: srv.URL, MaxRPS: 100},
		{ID: "raw", Title: "Без калибровки", URL: srv.URL, MaxRPS: 0},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := NewScheduler(ctx, settings, targets)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	return s, c
}

func okHandler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

// waitDone ждёт конца прогона и возвращает итог. С watch=true держит зрителя, чтобы
// прогон не отменился как брошенный.
func waitDone(t *testing.T, r *Run, watch bool) doneEvent {
	t.Helper()
	if watch {
		_, _, unsubscribe := r.subscribe()
		defer unsubscribe()
	}
	select {
	case <-r.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("прогон не закончился за 20 с")
	}
	history, _, _ := r.subscribe()
	last := history[len(history)-1]
	if last.name != "done" {
		t.Fatalf("последнее событие %q, ждали done", last.name)
	}
	var d doneEvent
	if err := json.Unmarshal(last.data, &d); err != nil {
		t.Fatalf("итог не разбирается: %v", err)
	}
	return d
}

// Цель с {domain} получает настоящий домен, а Save возвращает плейсхолдер на место.
func TestLoadAndSaveTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")
	src := `{"targets":[{"id":"home","title":"Главная","url":"https://{domain}/","max_rps":0}]}`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tg, err := LoadTargets(path, "example.ru")
	if err != nil {
		t.Fatalf("LoadTargets: %v", err)
	}
	if got := tg.Targets[0].URL; got != "https://example.ru/" {
		t.Errorf("url = %q, хотели https://example.ru/", got)
	}

	tg.Targets[0].MaxRPS = 120
	if err := tg.Save(path, "example.ru"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "https://{domain}/") || !strings.Contains(string(data), `"max_rps": 120`) {
		t.Errorf("после Save файл:\n%s", data)
	}
}

func TestLoadTargetsRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"нет целей":      `{"targets":[]}`,
		"плохой id":      `{"targets":[{"id":"Home!","url":"https://x.ru/"}]}`,
		"дубль":          `{"targets":[{"id":"a","url":"https://x.ru/"},{"id":"a","url":"https://x.ru/"}]}`,
		"не http":        `{"targets":[{"id":"a","url":"file:///etc/passwd"}]}`,
		"домен не задан": `{"targets":[{"id":"a","url":"https://{domain}/"}]}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "targets.json")
			_ = os.WriteFile(path, []byte(src), 0o644)
			// домен пустой: {domain} остаётся и должен быть пойман валидацией
			if _, err := LoadTargets(path, ""); err == nil {
				t.Error("ожидали ошибку")
			}
		})
	}
}

// Клиент не может передать свой URL: лишнее поле отклоняется целиком.
// Неизвестная, неоткалиброванная цель и доля вне границ тоже дают 400.
func TestRunRejectsBadRequests(t *testing.T) {
	s, _ := newTestScheduler(t, okHandler, testSettings())
	h := NewServer(s, "").Handler()

	cases := map[string]string{
		"чужой url":      `{"target_id":"ok","share":20,"duration_s":5,"url":"http://169.254.169.254/"}`,
		"неизвестная":    `{"target_id":"nope","share":20,"duration_s":5}`,
		"без калибровки": `{"target_id":"raw","share":20,"duration_s":5}`,
		"доля выше 50":   `{"target_id":"ok","share":60,"duration_s":5}`,
		"доля ниже 10":   `{"target_id":"ok","share":5,"duration_s":5}`,
		"слишком долго":  `{"target_id":"ok","share":20,"duration_s":16}`,
		"не json":        `target_id=ok`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("код %d, хотели 400; тело %s", rec.Code, rec.Body)
			}
		})
	}
	if st := s.Status(); st.Running {
		t.Error("после отклонённых запросов прогон не должен идти")
	}
}

// Один прогон за раз, пауза после него и лимит на IP: все три отказа это BusyError
// с временем ожидания, а по истечении паузы запуск снова возможен.
func TestSchedulerLimits(t *testing.T) {
	s, c := newTestScheduler(t, okHandler, testSettings())

	r1, err := s.Start("ok", 10, 1, "1.1.1.1")
	if err != nil {
		t.Fatalf("первый запуск: %v", err)
	}
	if r1.RPS != 10 {
		t.Errorf("RPS = %d, хотели 10%% от 100", r1.RPS)
	}

	var busy *BusyError
	if _, err := s.Start("ok", 10, 1, "2.2.2.2"); !errors.As(err, &busy) {
		t.Fatalf("во время прогона ждали BusyError, получили %v", err)
	}

	d := waitDone(t, r1, true)
	if d.Report.Total == 0 || d.Stopped != "" {
		t.Errorf("итог первого прогона: %+v", d)
	}

	// Пауза: прогон закончился «сейчас» по фальшивым часам.
	_, err = s.Start("ok", 10, 1, "2.2.2.2")
	if !errors.As(err, &busy) || busy.RetryAfter != 3*time.Minute {
		t.Fatalf("на паузе ждали BusyError с 3m, получили %v", err)
	}
	if st := s.Status(); st.NextRunInS != 180 || st.LastRunID != r1.ID {
		t.Errorf("статус на паузе: %+v", st)
	}

	// После паузы тот же IP запускает второй раз (лимит 2 за 15 минут)...
	c.advance(3 * time.Minute)
	r2, err := s.Start("ok", 10, 1, "1.1.1.1")
	if err != nil {
		t.Fatalf("после паузы: %v", err)
	}
	waitDone(t, r2, true)

	// ...а третий раз уже нет, хотя пауза прошла.
	c.advance(3 * time.Minute)
	_, err = s.Start("ok", 10, 1, "1.1.1.1")
	if !errors.As(err, &busy) || busy.RetryAfter != 9*time.Minute {
		t.Fatalf("лимит IP: ждали BusyError с 9m (15m минус 6m), получили %v", err)
	}
	// другой адрес при этом пускают
	r3, err := s.Start("ok", 10, 1, "3.3.3.3")
	if err != nil {
		t.Fatalf("другой IP: %v", err)
	}
	waitDone(t, r3, true)
}

// Цель отвечает 500: тормоз останавливает прогон задолго до конца и объясняет почему.
func TestBrakeStopsRun(t *testing.T) {
	s, _ := newTestScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, testSettings())

	r, err := s.Start("ok", 50, 10, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d := waitDone(t, r, true)

	if d.Stopped != StopBrake || !strings.Contains(d.Reason, "ошибок") {
		t.Errorf("итог %+v, ждали остановку тормозом из-за ошибок", d)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("прогон шёл %s, тормоз должен был сработать за пару секунд", took)
	}
}

// Если за прогоном никто не следит, он отменяется сам.
func TestAbandonedRunIsCancelled(t *testing.T) {
	settings := testSettings()
	settings.AbandonAfter = 250 * time.Millisecond
	s, _ := newTestScheduler(t, okHandler, settings)

	r, err := s.Start("ok", 10, 10, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	// Ждём конца без подписки: зритель удержал бы прогон.
	if d := waitDone(t, r, false); d.Stopped != StopAbandoned {
		t.Errorf("итог %+v, ждали stopped=%s", d, StopAbandoned)
	}
}

// SSE: живой зритель получает окна и итог, опоздавший после конца получает всю историю.
func TestEventsStream(t *testing.T) {
	s, _ := newTestScheduler(t, okHandler, testSettings())
	api := httptest.NewServer(NewServer(s, "").Handler())
	defer api.Close()

	resp, err := http.Post(api.URL+"/run", "application/json",
		strings.NewReader(`{"target_id":"ok","share":20,"duration_s":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		ID  string `json:"id"`
		RPS int    `json:"rps"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || run.ID == "" || run.RPS != 20 {
		t.Fatalf("POST /run: %d %+v", resp.StatusCode, run)
	}

	read := func() (windows int, done string) {
		resp, err := http.Get(api.URL + "/run/" + run.ID + "/events")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type %q", ct)
		}
		sc := bufio.NewScanner(resp.Body)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && name == "window":
				windows++
			case strings.HasPrefix(line, "data: ") && name == "done":
				done = strings.TrimPrefix(line, "data: ")
			}
		}
		return windows, done
	}

	live, done := read()
	if live < 1 || !strings.Contains(done, `"total"`) {
		t.Fatalf("живой поток: окон %d, итог %q", live, done)
	}
	late, lateDone := read()
	if late != live || lateDone != done {
		t.Errorf("опоздавший получил окон %d (живой %d), итог совпадает: %v", late, live, lateDone == done)
	}

	if resp, _ := http.Get(api.URL + "/run/unknown/events"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("неизвестный прогон: %d, хотели 404", resp.StatusCode)
	}
}

// X-Forwarded-For учитывается только от доверенного прокси, и берётся адрес,
// дописанный им самим (крайний правый).
func TestClientIP(t *testing.T) {
	srv := &Server{trustedProxy: "172.30.0.2"}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.30.0.2:51000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7")
	if got := srv.clientIP(r); got != "203.0.113.7" {
		t.Errorf("от прокси: %q, хотели 203.0.113.7", got)
	}

	r.RemoteAddr = "10.0.0.5:51000"
	if got := srv.clientIP(r); got != "10.0.0.5" {
		t.Errorf("не от прокси: %q, хотели 10.0.0.5 (заголовок игнорируется)", got)
	}
}
