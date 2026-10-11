package events

import (
	"fmt"
	"testing"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

func TestRingBufferFindAndStats(t *testing.T) {
	s := NewStore(100)
	now := time.Now()
	for i := 0; i < 150; i++ {
		sev := model.SeverityInfo
		if i%10 == 0 {
			sev = model.SeverityError
		}
		s.Add(model.Event{
			ID: fmt.Sprint(i), SourceID: "s", SourceName: "svc", ReceivedAt: now, Message: fmt.Sprintf("msg %d", i),
			Severity: sev, Category: "c", Fingerprint: map[bool]string{true: "err", false: "ok"}[sev == model.SeverityError],
		})
	}
	all := s.Find(Query{Limit: 1000})
	if len(all) != 100 || all[0].ID != "149" || all[99].ID != "50" {
		t.Fatalf("ring buffer: len=%d first=%s last=%s", len(all), all[0].ID, all[len(all)-1].ID)
	}
	errs := s.Find(Query{MinSeverity: model.SeverityError, Limit: 3})
	if len(errs) != 3 || errs[0].ID != "140" || errs[2].ID != "120" {
		t.Errorf("severity filter: %v", ids(errs))
	}
	older := s.Find(Query{MinSeverity: model.SeverityError, Before: "120"})
	if len(older) != 7 || older[0].ID != "110" {
		t.Errorf("cursor: %v", ids(older))
	}
	if got := s.Find(Query{Text: "MSG 14"}); len(got) != 10 { // 140-149
		t.Errorf("text search found %d", len(got))
	}
	if _, ok := s.Get("10"); ok {
		t.Error("rotated-out event should be gone")
	}

	st := s.Stats(now)
	if st.TotalSinceStart != 150 || st.LastHour != 150 || st.BySeverity[model.SeverityError] != 15 || len(st.Timeline) != 60 {
		t.Errorf("stats: total=%d hour=%d err=%d timeline=%d", st.TotalSinceStart, st.LastHour, st.BySeverity[model.SeverityError], len(st.Timeline))
	}
	if len(st.TopPatterns) != 2 || st.TopPatterns[0].Fingerprint != "err" || st.TopPatterns[0].Count != 15 {
		t.Errorf("top patterns should list errors first: %+v", st.TopPatterns)
	}
	if later := s.Stats(now.Add(2 * time.Hour)); later.LastHour != 0 || len(later.TopPatterns) != 0 {
		t.Errorf("stats should age out: %+v", later.LastHour)
	}
}

func ids(evs []model.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}
