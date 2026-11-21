// Package server exposes the engine over HTTP for cmd/riskd.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/engine"
	"github.com/victorcondackdev/fraud-rules-go/expr"
	"github.com/victorcondackdev/fraud-rules-go/internal/metrics"
)

// MaxBodyBytes bounds request bodies.
const MaxBodyBytes = 1 << 20

// KeyCounter reports the number of keys in the feature store, for metrics.
type KeyCounter interface{ Len() int }

// Server serves the risk API.
type Server struct {
	eng  *engine.Engine
	log  *slog.Logger
	reg  *metrics.Registry
	keys KeyCounter

	decisions  *metrics.CounterVec
	ruleHits   *metrics.CounterVec
	ruleErrors *metrics.CounterVec
	requests   *metrics.CounterVec
	events     *metrics.CounterVec
	reloads    *metrics.CounterVec
	latency    *metrics.Histogram
}

// New returns a server for eng. keys may be nil.
func New(eng *engine.Engine, keys KeyCounter, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	reg := &metrics.Registry{}
	s := &Server{eng: eng, log: log, reg: reg, keys: keys}
	s.decisions = reg.NewCounterVec("riskd_decisions_total", "Decisions by final action.", "action")
	s.ruleHits = reg.NewCounterVec("riskd_rule_hits_total", "Rule matches by rule and mode.", "rule", "mode")
	s.ruleErrors = reg.NewCounterVec("riskd_rule_errors_total", "Rules that could not be evaluated.", "rule", "fail_mode")
	s.requests = reg.NewCounterVec("riskd_http_requests_total", "HTTP requests by route and status code.", "route", "code")
	s.events = reg.NewCounterVec("riskd_events_recorded_total", "Settled transactions recorded into the feature store.")
	s.reloads = reg.NewCounterVec("riskd_ruleset_reloads_total", "Ruleset load attempts by result.", "result")
	s.latency = reg.NewHistogram("riskd_eval_duration_seconds", "Rule evaluation latency per transaction.",
		[]float64{5e-6, 10e-6, 25e-6, 50e-6, 100e-6, 250e-6, 500e-6, 1e-3, 2.5e-3, 10e-3})
	reg.NewGaugeFunc("riskd_ruleset_revision", "Revision of the active ruleset.", func() float64 {
		if c := eng.Active(); c != nil {
			return float64(c.Revision)
		}
		return 0
	})
	if keys != nil {
		reg.NewGaugeFunc("riskd_velocity_keys", "Keys held by the velocity feature store.", func() float64 { return float64(keys.Len()) })
	}
	return s
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h func(http.ResponseWriter, *http.Request) int) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
			code := h(w, r)
			s.requests.Inc(pattern, strconv.Itoa(code))
		})
	}
	route("POST /v1/evaluate", s.evaluate)
	route("POST /v1/events", s.recordEvents)
	route("GET /v1/rulesets", s.listRulesets)
	route("GET /v1/rulesets/active", s.activeRuleset)
	route("PUT /v1/rulesets", s.putRuleset)
	route("POST /v1/rulesets/rollback", s.rollback)
	route("POST /v1/rules/validate", s.validateRule)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = s.reg.WriteTo(w)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if s.eng.Active() == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "no ruleset"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// Metrics exposes the registry, for tests.
func (s *Server) Metrics() *metrics.Registry { return s.reg }

type apiError struct {
	Error    string    `json:"error"`
	Problems []problem `json:"problems,omitempty"`
}

type problem struct {
	RuleID  string `json:"rule_id,omitempty"`
	Index   int    `json:"index"`
	Field   string `json:"field"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Col     int    `json:"col,omitempty"`
	Offset  int    `json:"offset,omitempty"`
	End     int    `json:"end,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return code
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) int {
	return writeJSON(w, code, apiError{Error: fmt.Sprintf(format, args...)})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

func (s *Server) evaluate(w http.ResponseWriter, r *http.Request) int {
	var tx engine.Transaction
	if err := decode(r, &tx); err != nil {
		return writeErr(w, http.StatusBadRequest, "invalid transaction: %v", err)
	}
	if tx.ID == "" || tx.Time.IsZero() {
		return writeErr(w, http.StatusBadRequest, "invalid transaction: id and time are required")
	}
	d, err := s.eng.Evaluate(r.Context(), &tx)
	if err != nil {
		return writeErr(w, http.StatusServiceUnavailable, "%v", err)
	}
	s.observe(&d)
	return writeJSON(w, http.StatusOK, d)
}

func (s *Server) observe(d *engine.Decision) {
	s.latency.Observe(float64(d.ElapsedNs) / 1e9)
	s.decisions.Inc(d.Action.String())
	hits := make([]string, 0, len(d.Hits))
	for _, h := range d.Hits {
		s.ruleHits.Inc(h.RuleID, h.Mode.String())
		hits = append(hits, h.RuleID)
	}
	shadow := make([]string, 0, len(d.ShadowHits))
	for _, h := range d.ShadowHits {
		s.ruleHits.Inc(h.RuleID, h.Mode.String())
		shadow = append(shadow, h.RuleID)
	}
	for _, e := range d.Errors {
		s.ruleErrors.Inc(e.RuleID, e.FailMode.String())
	}
	attrs := []any{"tx", d.TransactionID, "action", d.Action.String(), "score", d.Score,
		"hits", hits, "ruleset", d.Ruleset, "revision", d.Revision, "elapsed_us", d.ElapsedNs / 1e3}
	if len(shadow) > 0 {
		attrs = append(attrs, "shadow_hits", shadow, "shadow_action", d.Shadow.Action.String())
	}
	if d.Degraded {
		attrs = append(attrs, "degraded", true, "errors", len(d.Errors))
		s.log.Warn("decision", attrs...)
		return
	}
	s.log.Info("decision", attrs...)
}

// recordEvents accepts one transaction object or an array of them.
func (s *Server) recordEvents(w http.ResponseWriter, r *http.Request) int {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return writeErr(w, http.StatusRequestEntityTooLarge, "reading body: %v", err)
	}
	var txs []engine.Transaction
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(body, &txs)
	} else {
		var tx engine.Transaction
		err = json.Unmarshal(body, &tx)
		txs = append(txs, tx)
	}
	if err != nil {
		return writeErr(w, http.StatusBadRequest, "invalid event: %v", err)
	}
	for i := range txs {
		if txs[i].Time.IsZero() || txs[i].Payer.ID == "" {
			return writeErr(w, http.StatusBadRequest, "event %d: time and payer.id are required", i)
		}
	}
	for i := range txs {
		s.eng.Record(&txs[i])
	}
	s.events.Add(uint64(len(txs)))
	return writeJSON(w, http.StatusAccepted, map[string]int{"recorded": len(txs)})
}

type rulesetInfo struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Revision int64     `json:"revision"`
	Digest   string    `json:"digest"`
	LoadedAt time.Time `json:"loaded_at"`
	Rules    int       `json:"rules"`
	Active   bool      `json:"active"`
}

func info(c *engine.Compiled, active *engine.Compiled) rulesetInfo {
	return rulesetInfo{Name: c.Ruleset.Name, Version: c.Ruleset.Version, Revision: c.Revision,
		Digest: c.Digest, LoadedAt: c.LoadedAt, Rules: len(c.Ruleset.Rules), Active: c == active}
}

func (s *Server) listRulesets(w http.ResponseWriter, _ *http.Request) int {
	active := s.eng.Active()
	hist := s.eng.History()
	out := make([]rulesetInfo, 0, len(hist))
	for i := len(hist) - 1; i >= 0; i-- {
		out = append(out, info(hist[i], active))
	}
	return writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
}

func (s *Server) activeRuleset(w http.ResponseWriter, _ *http.Request) int {
	c := s.eng.Active()
	if c == nil {
		return writeErr(w, http.StatusNotFound, "no ruleset loaded")
	}
	w.Header().Set("X-Ruleset-Revision", strconv.FormatInt(c.Revision, 10))
	return writeJSON(w, http.StatusOK, c.Ruleset)
}

func problems(err error) []problem {
	var ve *engine.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	out := make([]problem, len(ve.Problems))
	for i, p := range ve.Problems {
		out[i] = problem{RuleID: p.RuleID, Index: p.Index, Field: p.Field, Message: p.Err.Error()}
		var ee *expr.Error
		if errors.As(p.Err, &ee) {
			out[i].Message = ee.Msg
			out[i].Line, out[i].Col, out[i].Offset, out[i].End = ee.Pos.Line, ee.Pos.Col, ee.Pos.Offset, ee.End
			out[i].Snippet = ee.Snippet()
		}
	}
	return out
}

// putRuleset validates the body and atomically activates it. Evaluations
// in flight finish on the previous revision.
func (s *Server) putRuleset(w http.ResponseWriter, r *http.Request) int {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return writeErr(w, http.StatusRequestEntityTooLarge, "reading body: %v", err)
	}
	c, err := s.eng.LoadJSON(body)
	if err != nil {
		s.reloads.Inc("invalid")
		s.log.Warn("ruleset rejected", "error", err)
		return writeJSON(w, http.StatusUnprocessableEntity, apiError{Error: err.Error(), Problems: problems(err)})
	}
	s.reloads.Inc("ok")
	return writeJSON(w, http.StatusOK, info(c, s.eng.Active()))
}

func (s *Server) rollback(w http.ResponseWriter, _ *http.Request) int {
	c, err := s.eng.Rollback()
	if err != nil {
		return writeErr(w, http.StatusConflict, "%v", err)
	}
	s.reloads.Inc("rollback")
	return writeJSON(w, http.StatusOK, info(c, s.eng.Active()))
}

type validateRequest struct {
	Expr string `json:"expr"`
}

type validateResponse struct {
	Valid      bool     `json:"valid"`
	Type       string   `json:"type,omitempty"`
	Cost       int      `json:"cost,omitempty"`
	Features   []string `json:"features,omitempty"`
	Aggregates []string `json:"aggregates,omitempty"`
	Error      *problem `json:"error,omitempty"`
}

// validateRule type-checks one expression against the Pix schema, for
// editors that want inline diagnostics.
func (s *Server) validateRule(w http.ResponseWriter, r *http.Request) int {
	var req validateRequest
	if err := decode(r, &req); err != nil {
		return writeErr(w, http.StatusBadRequest, "invalid request: %v", err)
	}
	opts := expr.Options{MaxWindow: s.eng.CompileOptions().MaxWindow}
	p, err := expr.Compile(req.Expr, engine.Schema(), opts)
	if err != nil {
		pr := &problem{Field: "expr", Message: err.Error()}
		var ee *expr.Error
		if errors.As(err, &ee) {
			pr.Message = ee.Msg
			pr.Line, pr.Col, pr.Offset, pr.End = ee.Pos.Line, ee.Pos.Col, ee.Pos.Offset, ee.End
			pr.Snippet = ee.Snippet()
		}
		return writeJSON(w, http.StatusOK, validateResponse{Error: pr})
	}
	resp := validateResponse{Valid: p.Type() == expr.Bool, Type: p.Type().String(), Cost: p.Cost()}
	for _, f := range p.Features() {
		resp.Features = append(resp.Features, f.Name)
	}
	for _, a := range p.Aggregates() {
		resp.Aggregates = append(resp.Aggregates, a.String())
	}
	if !resp.Valid {
		resp.Error = &problem{Field: "expr", Message: "rule expressions must be bool, got " + p.Type().String(), Line: 1, Col: 1}
	}
	return writeJSON(w, http.StatusOK, resp)
}
