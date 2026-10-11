package importer

import (
	"sort"
	"time"

	"github.com/vipinmohan22/layalogs/internal/model"
)

const (
	maxTimelineKeys = 10000 // buckets held before coarsening
	timelinePoints  = 120   // points in the charted timeline
)

// resolutions are the bucket sizes the aggregate coarsens through; each is
// a multiple of the previous so counts move between levels exactly.
var resolutions = []int64{60, 3600, 86400, 30 * 86400}

// niceSteps are the bucket sizes a chart may use.
var niceSteps = []int64{60, 300, 900, 1800, 3600, 3 * 3600, 6 * 3600, 12 * 3600, 86400, 7 * 86400, 30 * 86400, 90 * 86400, 360 * 86400}

// Timeline is a chartable count of events by severity over log time.
type Timeline struct {
	Points []TimelinePoint `json:"points"`
	Step   int64           `json:"step_seconds,omitempty"`
	Start  time.Time       `json:"start,omitzero"`
	End    time.Time       `json:"end,omitzero"`
	// Events outside the charted range: outlier timestamps (a stray
	// default date, a far-future value) are left out so they don't flatten
	// the chart.
	Before int64 `json:"before,omitempty"`
	After  int64 `json:"after,omitempty"`
}

// TimelinePoint counts events by severity in one time bucket.
type TimelinePoint struct {
	Time       time.Time                `json:"time"`
	Total      int64                    `json:"total"`
	BySeverity map[model.Severity]int64 `json:"by_severity"`
}

// timeAgg counts events per time bucket by severity rank. It starts at one
// minute resolution and coarsens through resolutions whenever it holds more
// than maxTimelineKeys buckets, so memory stays bounded for any span.
type timeAgg struct {
	level   int
	buckets map[int64]*[8]int64
}

func newTimeAgg() timeAgg {
	return timeAgg{buckets: map[int64]*[8]int64{}}
}

func (t *timeAgg) res() int64 { return resolutions[t.level] }

func (t *timeAgg) add(unix int64, rank int) {
	key := floorDiv(unix, t.res()) * t.res()
	b := t.buckets[key]
	if b == nil {
		for len(t.buckets) >= maxTimelineKeys && t.level < len(resolutions)-1 {
			t.coarsen()
			key = floorDiv(unix, t.res()) * t.res()
		}
		if b = t.buckets[key]; b == nil {
			b = new([8]int64)
			t.buckets[key] = b
		}
	}
	b[rank]++
}

func (t *timeAgg) coarsen() {
	t.level++
	res := t.res()
	next := make(map[int64]*[8]int64, len(t.buckets)/8+1)
	for k, b := range t.buckets {
		nk := floorDiv(k, res) * res
		dst := next[nk]
		if dst == nil {
			dst = new([8]int64)
			next[nk] = dst
		}
		for i, v := range b {
			dst[i] += v
		}
	}
	t.buckets = next
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// snapshot builds the chart. Outlier timestamps (a stray 2000-01-01 from a
// device without a clock, a far-future epoch) would squash the real activity
// into one bar, so buckets more than one "bulk span" away from the middle
// 99% of events are left out and counted separately. Natural edges of the
// data are never cut. It then picks a step so the range fits timelinePoints.
func (t *timeAgg) snapshot() Timeline {
	if len(t.buckets) == 0 {
		return Timeline{}
	}
	keys := make([]int64, 0, len(t.buckets))
	var total int64
	for k, b := range t.buckets {
		keys = append(keys, k)
		for _, v := range b {
			total += v
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	res := t.res()
	lo, hi := keys[0], keys[len(keys)-1]
	if total >= 200 {
		// The middle 99% of events…
		trim := total / 200
		blo, bhi := lo, hi
		var acc int64
		for _, k := range keys {
			if acc += sum(t.buckets[k]); acc > trim {
				blo = k
				break
			}
		}
		acc = 0
		for i := len(keys) - 1; i >= 0; i-- {
			if acc += sum(t.buckets[keys[i]]); acc > trim {
				bhi = keys[i]
				break
			}
		}
		// …widened by its own span on each side; anything beyond is an outlier.
		margin := bhi - blo + res
		lo, hi = keys[len(keys)-1], keys[0]
		for _, k := range keys {
			if k >= blo-margin && k <= bhi+margin {
				lo, hi = min(lo, k), max(hi, k)
			}
		}
	}
	span := hi + res - lo
	step := int64(0)
	for _, s := range niceSteps {
		if s%res == 0 && s*timelinePoints >= span {
			step = s
			break
		}
	}
	if step == 0 {
		step = res * ((span + res*timelinePoints - 1) / (res * timelinePoints))
	}
	start := floorDiv(lo, step) * step
	n := floorDiv(hi-start, step) + 1
	tl := Timeline{Step: step, Start: time.Unix(start, 0).UTC(), End: time.Unix(start+n*step, 0).UTC()}
	tl.Points = make([]TimelinePoint, n)
	for i := range tl.Points {
		tl.Points[i] = TimelinePoint{Time: time.Unix(start+int64(i)*step, 0).UTC(), BySeverity: map[model.Severity]int64{}}
	}
	for _, k := range keys {
		b := t.buckets[k]
		switch {
		case k < lo:
			tl.Before += sum(b)
			continue
		case k > hi:
			tl.After += sum(b)
			continue
		}
		p := &tl.Points[floorDiv(k-start, step)]
		for rank, v := range b {
			if v > 0 {
				p.Total += v
				p.BySeverity[sevForRank(rank)] += v
			}
		}
	}
	return tl
}

func sum(b *[8]int64) int64 {
	var n int64
	for _, v := range b {
		n += v
	}
	return n
}

func sevForRank(rank int) model.Severity {
	if rank >= 1 && rank <= len(model.Severities) {
		return model.Severities[rank-1]
	}
	return "unknown"
}
