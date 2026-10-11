package importer

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/vipinmohan22/layalogs/internal/model"
)

// retainer keeps a bounded sample of events for browsing, within a count
// and an approximate byte budget.
//
// Up to a fifth of the budget is reserved for samples: the first couple of
// events of each pattern, so every pattern in the report can be opened even
// when it is rare. The rest is filled by severity: once full, a new event
// evicts the most recently kept events of lower severity, or is dropped if
// that cannot free enough room.
type retainer struct {
	maxCount int
	maxBytes int64

	seq     int64
	samples []retained
	sBytes  int64
	general [8][]retained // indexed by Severity.Rank()
	gCount  int
	gBytes  [8]int64
}

type retained struct {
	seq  int64
	cost int64
	ev   model.Event
}

func (r *retainer) count() int {
	return len(r.samples) + r.gCount
}

func (r *retainer) bytes() int64 {
	b := r.sBytes
	for _, v := range r.gBytes {
		b += v
	}
	return b
}

// add offers ev for retention; sample marks it as one of the first events
// of its pattern. It reports whether ev was kept as a sample.
func (r *retainer) add(ev *model.Event, sample bool) bool {
	if r.maxCount <= 0 {
		return false
	}
	r.seq++
	it := retained{seq: r.seq, ev: compact(*ev)}
	it.cost = cost(&it.ev)

	if sample && len(r.samples) < r.maxCount/5 && r.sBytes+it.cost <= r.maxBytes/5 {
		r.samples = append(r.samples, it)
		r.sBytes += it.cost
		r.makeRoom(0, 0) // samples may push the general pool over budget
		return true
	}
	rank := ev.Severity.Rank()
	if !r.fits(1, it.cost) {
		// Can evicting lower-severity events make enough room?
		freeCount, freeBytes := 0, int64(0)
		for low := 0; low < rank; low++ {
			freeCount += len(r.general[low])
			freeBytes += r.gBytes[low]
		}
		if !r.fitsAfter(1, it.cost, freeCount, freeBytes) {
			return false
		}
		r.makeRoom(1, it.cost)
	}
	r.general[rank] = append(r.general[rank], it)
	r.gCount++
	r.gBytes[rank] += it.cost
	return false
}

func (r *retainer) fits(addCount int, addBytes int64) bool {
	return r.fitsAfter(addCount, addBytes, 0, 0)
}

func (r *retainer) fitsAfter(addCount int, addBytes int64, freeCount int, freeBytes int64) bool {
	return r.count()+addCount-freeCount <= r.maxCount && r.bytes()+addBytes-freeBytes <= r.maxBytes
}

// makeRoom evicts general events, lowest severity and most recent first,
// until addCount events of addBytes would fit.
func (r *retainer) makeRoom(addCount int, addBytes int64) {
	for rank := 0; rank < len(r.general) && !r.fits(addCount, addBytes); rank++ {
		for len(r.general[rank]) > 0 && !r.fits(addCount, addBytes) {
			last := len(r.general[rank]) - 1
			r.gBytes[rank] -= r.general[rank][last].cost
			r.general[rank][last] = retained{}
			r.general[rank] = r.general[rank][:last]
			r.gCount--
		}
	}
}

func (r *retainer) each(fn func(ev *model.Event)) {
	for i := range r.samples {
		fn(&r.samples[i].ev)
	}
	for rank := range r.general {
		for i := range r.general[rank] {
			fn(&r.general[rank][i].ev)
		}
	}
}

// ordered returns kept events in arrival (file) order.
func (r *retainer) ordered() []model.Event {
	all := make([]retained, 0, r.count())
	all = append(all, r.samples...)
	for _, list := range r.general {
		all = append(all, list...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	out := make([]model.Event, len(all))
	for i, it := range all {
		out[i] = it.ev
	}
	return out
}

// compact shortens a copy of ev for retention: long messages are cut and
// very large structured fields are dropped. Strings are copied so a kept
// event never pins the larger record it was cut from.
func compact(ev model.Event) model.Event {
	ev.Message = strings.Clone(model.Truncate(ev.Message, retainedMsgBytes))
	ev.Level = strings.Clone(model.Truncate(ev.Level, 64))
	ev.Summary = strings.Clone(model.Truncate(ev.Summary, retainedMsgBytes))
	if len(ev.Fields) > 0 && fieldsCost(ev.Fields, retainedFieldsBytes) > retainedFieldsBytes {
		ev.Fields = map[string]any{"laya_fields_omitted": "structured fields were too large to keep for browsing"}
	}
	return ev
}

// cost approximates the memory a retained event holds.
func cost(ev *model.Event) int64 {
	n := int64(300 + len(ev.Message) + len(ev.Summary) + len(ev.Level))
	for _, t := range ev.Tags {
		n += int64(len(t)) + 16
	}
	for _, t := range ev.MatchedRules {
		n += int64(len(t)) + 16
	}
	return n + fieldsCost(ev.Fields, 1<<30)
}

// fieldsCost estimates the size of decoded JSON, stopping early past limit.
func fieldsCost(v any, limit int64) int64 {
	var n int64
	var walk func(v any)
	walk = func(v any) {
		if n > limit {
			return
		}
		switch t := v.(type) {
		case map[string]any:
			n += 48
			for k, x := range t {
				n += int64(len(k)) + 32
				walk(x)
			}
		case []any:
			n += 24
			for _, x := range t {
				walk(x)
			}
		case string:
			n += int64(len(t)) + 16
		case json.Number:
			n += int64(len(t)) + 16
		default:
			n += 16
		}
	}
	walk(v)
	return n
}
