package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server это HTTP-API формы. Снаружи живёт под /loadgen/api/ на домене сайта, но
// префикс срезает прокси, поэтому маршруты здесь от корня.
type Server struct {
	sched *Scheduler
	// trustedProxy это адрес прокси (Caddy), которому верим в X-Forwarded-For.
	// Для всех остальных берём адрес TCP-соединения: заголовок подделать легко.
	trustedProxy string
	heartbeat    time.Duration
}

func NewServer(sched *Scheduler, trustedProxy string) *Server {
	return &Server{sched: sched, trustedProxy: trustedProxy, heartbeat: 15 * time.Second}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /targets", s.handleTargets)
	mux.HandleFunc("POST /run", s.handleRun)
	mux.HandleFunc("GET /run/{id}/events", s.handleEvents)
	return mux
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sched.Status())
}

// targetView это то, что форма знает о цели. URL наружу не отдаём: он не нужен
// форме и только подсказал бы, куда ещё можно постучаться.
type targetView struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	MaxRPS int    `json:"max_rps"`
}

type targetsResponse struct {
	Targets      []targetView `json:"targets"`
	MinShare     int          `json:"min_share"`
	MaxShare     int          `json:"max_share"`
	MaxDurationS int          `json:"max_duration_s"`
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	resp := targetsResponse{
		MinShare:     s.sched.settings.MinShare,
		MaxShare:     s.sched.settings.MaxShare,
		MaxDurationS: int(s.sched.settings.MaxDuration.Seconds()),
	}
	for _, t := range s.sched.targets.Targets {
		// неоткалиброванные цели форме не показываем
		if t.MaxRPS > 0 {
			resp.Targets = append(resp.Targets, targetView{ID: t.ID, Title: t.Title, MaxRPS: t.MaxRPS})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// runRequest это всё, что клиент может сказать о прогоне. Неизвестные поля, например
// url, отклоняются целиком: так случайно не появится способ передать свою цель.
type runRequest struct {
	TargetID  string `json:"target_id"`
	Share     int    `json:"share"`
	DurationS int    `json:"duration_s"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный запрос: ожидаются только target_id, share и duration_s")
		return
	}

	run, err := s.sched.Start(req.TargetID, req.Share, req.DurationS, s.clientIP(r))
	var busy *BusyError
	switch {
	case errors.As(err, &busy):
		retry := ceilSeconds(busy.RetryAfter)
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": busy.Message, "retry_after_s": retry})
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

// handleEvents это SSE-поток прогона: сначала накопленная история (опоздавший зритель
// видит график с начала), потом живые окна раз в секунду и в конце итог.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	run, ok := s.sched.Find(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "прогон не найден")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "потоковая отдача не поддерживается")
		return
	}

	history, ch, unsubscribe := run.subscribe()
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	for _, e := range history {
		writeEvent(w, e)
	}
	flusher.Flush()

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return // зритель ушёл; если ушли все, прогон отменит watchViewers
		case e, open := <-ch:
			if !open {
				return
			}
			writeEvent(w, e)
			flusher.Flush()
		case <-heartbeat.C:
			// комментарий SSE не даёт прокси закрыть «молчащее» соединение
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e event) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
}

// clientIP берёт адрес посетителя для лимита на IP. X-Forwarded-For учитываем, только
// если соединение пришло от нашего прокси: Caddy перезаписывает этот заголовок
// адресом клиента, а от кого-то другого он мог прийти подделанным.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.trustedProxy == "" || host != s.trustedProxy {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	// Берём крайний правый адрес: его дописал наш прокси, остальное мог прислать клиент.
	parts := strings.Split(xff, ",")
	return strings.TrimSpace(parts[len(parts)-1])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
