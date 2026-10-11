// Package enginetest provides a fake laya-serve for tests. It follows the
// contract of laya-serve 0.4.2 (laya/serve.py): POST /v1/systemone with
// {state, questions, model}, answers keyed by question id, usage and
// routing, {"detail": ...} errors, bearer auth, and unknown model names
// silently routed to the default checkpoint.
package enginetest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Server is a fake laya-serve. Its answers are chosen by Decide, which tests
// may replace; by default a choice question picks the first label whose
// name appears in the state (or the last label), with probability 0.9.
type Server struct {
	*httptest.Server

	Key     string   // required bearer key, if set
	Models  []string // checkpoints it serves; others route to Models[0]
	Decide  func(state string, q Question) Answer
	Busy    atomic.Int32 // answer 503 to this many requests
	Fail    atomic.Int32 // answer 500 to this many requests
	Calls   atomic.Int32
	mu      sync.Mutex
	Last    Request // the last decision request
	Headers http.Header
}

// Request is what the fake received.
type Request struct {
	State     any                        `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
	Model     string                     `json:"model"`
	// Order lists each choice question's labels in the order sent.
	Order map[string][]string `json:"-"`
}

// Question is one decoded question.
type Question struct {
	ID           string
	Type         string
	Instructions string
	Labels       []string // choice labels in order, or score levels
}

// Answer is the fake's answer to one question.
type Answer struct {
	Choice string
	Probs  map[string]float64
}

// New starts a fake serving the english and multilingual checkpoints.
func New() *Server {
	s := &Server{Models: []string{"english", "multilingual"}}
	s.Decide = s.defaultDecide
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /v1/systemone", s.systemone)
	s.Server = httptest.NewServer(mux)
	return s
}

// LastRequest returns the last decision request.
func (s *Server) LastRequest() Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Last
}

func (s *Server) defaultDecide(state string, q Question) Answer {
	pick := q.Labels[len(q.Labels)-1]
	for _, l := range q.Labels {
		if strings.Contains(strings.ToLower(state), strings.ToLower(l)) {
			pick = l
			break
		}
	}
	probs := map[string]float64{}
	for _, l := range q.Labels {
		probs[l] = 0.1 / float64(max(len(q.Labels)-1, 1))
	}
	probs[pick] = 0.9
	return Answer{Choice: pick, Probs: probs}
}

func detail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"detail": msg})
}

func (s *Server) authorized(r *http.Request) bool {
	return s.Key == "" || r.Header.Get("Authorization") == "Bearer "+s.Key
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}
	revs := map[string]string{}
	for _, m := range s.Models {
		revs[m] = "7b928d8"
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "loaded": s.Models, "revisions": revs, "device": "cpu"})
}

func (s *Server) systemone(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		detail(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return
	}
	if s.Busy.Load() > 0 {
		s.Busy.Add(-1)
		w.Header().Set("Retry-After", "1")
		detail(w, http.StatusServiceUnavailable, "server busy, try again later")
		return
	}
	s.Calls.Add(1)
	if s.Fail.Load() > 0 {
		s.Fail.Add(-1)
		detail(w, http.StatusInternalServerError, "inference failed")
		return
	}
	var buf bytes.Buffer
	buf.ReadFrom(r.Body)
	var req Request
	if err := json.Unmarshal(buf.Bytes(), &req); err != nil {
		detail(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	if req.Questions == nil {
		detail(w, http.StatusBadRequest, "request body must be an object with a 'questions' field")
		return
	}
	if req.State == nil {
		detail(w, http.StatusBadRequest, "'state' is required")
		return
	}
	state, ok := req.State.(string)
	if !ok {
		b, _ := json.Marshal(req.State)
		state = string(b)
	}
	if len(state) > 50000 {
		detail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("state too large (%d > 50000 chars)", len(state)))
		return
	}
	model := s.Models[0]
	for _, m := range s.Models {
		if m == req.Model {
			model = m
		}
	}
	req.Order = map[string][]string{}
	answers := map[string]any{}
	for id, raw := range req.Questions {
		q, err := decodeQuestion(id, raw)
		if err != nil {
			detail(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		req.Order[id] = q.Labels
		a := s.Decide(state, q)
		best := 0.0
		for _, p := range a.Probs {
			best = max(best, p)
		}
		switch q.Type {
		case "choice":
			answers[id] = map[string]any{"type": "choice", "choice": a.Choice, "probabilities": a.Probs,
				"confidence": 0.5, "answer_confidence": best, "action": map[string]float64{"act_probability": 1}, "x_jev_confidence": 0.5}
		case "score":
			probs := map[string]float64{}
			exp := 0.0
			for i, l := range q.Labels {
				k := strconv.Itoa(i)
				probs[k] = a.Probs[l]
				exp += float64(i) * a.Probs[l]
			}
			legend := map[string]string{}
			for i, l := range q.Labels {
				legend[strconv.Itoa(i)] = l
			}
			answers[id] = map[string]any{"type": "score", "score": exp, "legend": legend, "probabilities": probs,
				"confidence": 0.5, "answer_confidence": best, "action": map[string]float64{"act_probability": 1}}
		case "noul":
			p := a.Probs["true"]
			answers[id] = map[string]any{"type": "noul", "noul": p, "confidence": max(p, 1-p), "answer_confidence": max(p, 1-p)}
		}
	}
	s.mu.Lock()
	s.Last, s.Headers = req, r.Header.Clone()
	s.mu.Unlock()
	truncated := len(state) > 1800
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Inference-Time-Ms", "12.5")
	json.NewEncoder(w).Encode(map[string]any{
		"model":   "laya-rl-agent",
		"answers": answers,
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 0, "state_tokens": len(state) / 4,
			"state_tokens_dropped": 0, "truncated": truncated, "truncated_questions": []string{}},
		"routing": map[string]any{"model": model, "repo": "convaiinnovations/laya", "reason": "pinned", "detection": nil, "workflow": nil},
	})
}

// decodeQuestion validates a question like laya-serve does and returns its
// labels in the order they were sent.
func decodeQuestion(id string, raw json.RawMessage) (Question, error) {
	var q struct {
		Type         string          `json:"type"`
		Instructions string          `json:"instructions"`
		Criteria     json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return Question{}, fmt.Errorf("question '%s': %v", id, err)
	}
	out := Question{ID: id, Type: q.Type, Instructions: q.Instructions}
	if strings.TrimSpace(q.Instructions) == "" {
		return out, fmt.Errorf("question '%s': instructions must be a non-empty string", id)
	}
	switch q.Type {
	case "choice":
		dec := json.NewDecoder(bytes.NewReader(q.Criteria))
		if t, err := dec.Token(); err != nil || t != json.Delim('{') {
			return out, fmt.Errorf("question '%s': choice criteria must be an object", id)
		}
		for dec.More() {
			t, _ := dec.Token()
			out.Labels = append(out.Labels, t.(string))
			var v any
			dec.Decode(&v)
		}
		if len(out.Labels) == 0 {
			return out, fmt.Errorf("question '%s': choice criteria must not be empty", id)
		}
	case "score":
		if err := json.Unmarshal(q.Criteria, &out.Labels); err != nil || len(out.Labels) == 0 {
			return out, fmt.Errorf("question '%s': score criteria must be a non-empty list", id)
		}
	case "noul":
		out.Labels = []string{"false", "true"}
	default:
		return out, fmt.Errorf("question '%s': unknown type '%s'; use one of ['choice', 'noul', 'score']", id, q.Type)
	}
	return out, nil
}
