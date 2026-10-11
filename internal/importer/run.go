package importer

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/ingest"
	"github.com/vipinmohan22/layalogs/internal/model"
)

// Options configures one import.
type Options struct {
	FileName       string
	Source         config.Source  // parse settings, name and tags applied to events
	Format         string         // overrides the source's parse format when set
	Location       *time.Location // zone for timestamps without one (default UTC)
	MaxBytes       int64          // cap on decompressed input; 0 means none
	MaxRecordBytes int64          // cap per JSON value; 0 means ingest's default, negative none
	Deadline       time.Time      // stop (as truncated) at this time; zero means none
	Workers        int            // parallel classification; <= 1 classifies inline
}

// Deps connects an import to classification.
type Deps struct {
	// Classify classifies ev in place and returns false if a rule drops it.
	// With Workers > 1 it is called concurrently.
	Classify func(ev *model.Event) bool
	// Routes, when set, returns the alert routes ev would match. Nothing is
	// sent; the report counts them.
	Routes func(ev *model.Event) []string
	// Busy, when set, reports that live ingest is backed up; the import
	// pauses until it isn't.
	Busy func() bool
	// OnEvent, when set, receives each kept event in order; an error stops
	// the import.
	OnEvent func(ev model.Event) error
}

// StopError ends an import early as truncated with the given reason, keeping
// what was analyzed so far. Readers passed to Run may return it.
type StopError struct {
	Reason string
	Err    error
}

func (e *StopError) Error() string { return e.Err.Error() }
func (e *StopError) Unwrap() error { return e.Err }

var errLimit = errors.New("the decompressed size limit was reached")

// Fail finishes an import that could not start (for example, invalid
// request options) as failed.
func Fail(im *Import, msg string) {
	im.finish(StatusFailed, StopInvalid, msg, time.Now().UTC())
}

const batchSize = 256

// Run streams r through parsing and classification into im and marks it
// finished; it never leaves im running, even if a dependency panics. Early
// stops (size limits, deadline, client abort, malformed data part-way)
// finish as done+truncated with partial results; unsupported input and
// internal errors finish as failed. The returned error is nil for a clean
// or truncated finish.
func Run(ctx context.Context, im *Import, r io.Reader, opts Options, deps Deps) (err error) {
	tz := "UTC"
	if opts.Location != nil {
		tz = opts.Location.String()
	}
	im.update(func(rep *Report) {
		rep.SourceID, rep.SourceName, rep.TimeZone = opts.Source.ID, opts.Source.Name, tz
		if opts.FileName != "" {
			rep.FileName = opts.FileName // the upload's name may only be known now
		}
	})
	if !opts.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, opts.Deadline)
		defer cancel()
	}
	// Deferred calls run last-first: recover records a panic, then the safety
	// net guarantees the import never stays running.
	defer func() {
		im.finish(StatusFailed, StopInternal, "the analysis ended unexpectedly", time.Now().UTC())
	}()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("internal error: %v", p)
			im.finish(StatusFailed, StopInternal, err.Error(), time.Now().UTC())
		}
	}()

	runErr := run(ctx, im, r, opts, deps)
	status, reason, msg := classifyStop(ctx, im, runErr)
	im.finish(status, reason, msg, time.Now().UTC())
	if status == StatusFailed {
		return runErr
	}
	return nil
}

// classifyStop maps how a run ended onto a status and stop reason.
func classifyStop(ctx context.Context, im *Import, err error) (status, reason, msg string) {
	if err == nil {
		return StatusDone, "", ""
	}
	rep := im.Report(false)
	var stop *StopError
	var unsupported *ingest.UnsupportedError
	var syntax *ingest.SyntaxError
	switch {
	case errors.As(err, &unsupported):
		return StatusFailed, StopUnsupported, err.Error()
	case errors.As(err, &stop):
		return StatusDone, stop.Reason, err.Error()
	case errors.Is(err, errLimit):
		return StatusDone, StopDecompressedLimit, err.Error()
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return StatusDone, StopDeadline, "the analysis took too long and was stopped"
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return StatusDone, StopClientAborted, "the upload was interrupted"
	case errors.As(err, &syntax):
		return StatusDone, StopSyntax, err.Error()
	case errors.Is(err, ingest.ErrRecordTooLarge):
		return StatusDone, StopRecordTooLarge, err.Error()
	case rep.Compressed && errors.Is(err, io.ErrUnexpectedEOF):
		return StatusDone, StopGzipTruncated, "the gzip data ended early (truncated file?)"
	case rep.Events > 0 || rep.Dropped > 0:
		return StatusDone, StopReadError, err.Error()
	}
	return StatusFailed, StopReadError, err.Error()
}

func run(ctx context.Context, im *Import, r io.Reader, opts Options, deps Deps) error {
	raw := &rawCounter{r: r}
	br := bufio.NewReader(raw)
	var input io.Reader = br
	counter := &countingReader{limit: opts.MaxBytes, im: im}
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return &ingest.UnsupportedError{Reason: "invalid gzip data: " + err.Error()}
		}
		defer gz.Close()
		input = gz
		counter.compressed = raw
		im.update(func(rep *Report) { rep.Compressed = true })
	}
	counter.r = input

	b := &batcher{ctx: ctx, im: im, deps: deps, workers: opts.Workers, fileName: opts.FileName}
	stats, err := ingest.Stream(counter, &opts.Source, im.start, ingest.StreamOptions{
		Format:         opts.Format,
		FileName:       opts.FileName,
		Location:       opts.Location,
		MaxRecordBytes: opts.MaxRecordBytes,
	}, b.push)
	// Record what was parsed before any stop, so partial reports are whole.
	if ferr := b.flushFinal(); err == nil {
		err = ferr
	}
	im.update(func(rep *Report) {
		rep.Parse = stats
		if stats.Lines > rep.Lines {
			rep.Lines = stats.Lines
		}
	})
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// batcher classifies events in batches (in parallel when workers > 1) and
// records them in order.
type batcher struct {
	ctx      context.Context
	im       *Import
	deps     Deps
	workers  int
	fileName string
	batch    []model.Event
	pushed   int
}

func (b *batcher) push(ev model.Event) error {
	ev.ImportID = b.im.ID()
	ev.File = b.fileName
	b.batch = append(b.batch, ev)
	if len(b.batch) >= batchSize {
		return b.flush()
	}
	return nil
}

func (b *batcher) flush() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if err := b.yield(); err != nil {
		return err
	}
	return b.process()
}

// flushFinal records the last partial batch even after a stop or
// cancellation: those events were already read, and a stopped import should
// report everything up to the stop.
func (b *batcher) flushFinal() error {
	if b.ctx.Err() == nil {
		_ = b.yield()
	}
	return b.process()
}

func (b *batcher) process() error {
	if len(b.batch) == 0 {
		return nil
	}
	kept := make([]bool, len(b.batch))
	if b.workers > 1 && len(b.batch) > 1 {
		var wg sync.WaitGroup
		var panicked atomic.Pointer[any]
		chunk := (len(b.batch) + b.workers - 1) / b.workers
		for start := 0; start < len(b.batch); start += chunk {
			end := min(start+chunk, len(b.batch))
			wg.Add(1)
			go func(lo, hi int) {
				defer wg.Done()
				defer func() {
					if p := recover(); p != nil {
						panicked.CompareAndSwap(nil, &p)
					}
				}()
				for i := lo; i < hi; i++ {
					kept[i] = b.deps.Classify(&b.batch[i])
				}
			}(start, end)
		}
		wg.Wait()
		if p := panicked.Load(); p != nil {
			panic(*p) // on the calling goroutine, where Run recovers it
		}
	} else {
		for i := range b.batch {
			kept[i] = b.deps.Classify(&b.batch[i])
		}
	}
	for i := range b.batch {
		ev := &b.batch[i]
		var routes []string
		if kept[i] && b.deps.Routes != nil {
			routes = b.deps.Routes(ev)
		}
		b.im.add(ev, kept[i], routes)
		if kept[i] && b.deps.OnEvent != nil {
			if err := b.deps.OnEvent(*ev); err != nil {
				b.reset() // these events were recorded; never replay them
				return &StopError{Reason: StopOutput, Err: fmt.Errorf("writing the output failed: %w", err)}
			}
		}
	}
	b.pushed += len(b.batch)
	b.reset()
	return nil
}

func (b *batcher) reset() {
	clear(b.batch)
	b.batch = b.batch[:0]
}

// yield pauses while live ingest is backed up, so a large import never
// crowds out live logs.
func (b *batcher) yield() error {
	if b.deps.Busy == nil {
		return nil
	}
	start := time.Now()
	for b.deps.Busy() {
		select {
		case <-b.ctx.Done():
			return b.ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if waited := time.Since(start); waited >= time.Millisecond {
		b.im.update(func(rep *Report) { rep.YieldedMs += waited.Milliseconds() })
	}
	return nil
}

// Decompression bombs: real logs compress 5-30x. Past this many bytes,
// output more than maxRatio times the compressed input read so far stops
// the import.
const (
	ratioFloor = 64 << 20
	maxRatio   = 100
)

var errRatio = &StopError{Reason: StopRatioLimit, Err: errors.New("the file decompresses to more than 100 times its size; stopped as a likely decompression bomb")}

// rawCounter counts bytes read from the upload itself.
type rawCounter struct {
	r io.Reader
	n int64
}

func (c *rawCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// countingReader tracks decompressed bytes into the report and enforces the
// size cap and, for compressed input, the ratio check.
type countingReader struct {
	r          io.Reader
	limit      int64
	n          int64
	compressed *rawCounter // set for gzip input
	im         *Import
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.compressed != nil && c.n > ratioFloor && c.n > maxRatio*c.compressed.n {
		return 0, errRatio
	}
	if c.limit > 0 {
		if c.n >= c.limit {
			// At the limit: it was only reached if more input follows.
			var one [1]byte
			if _, err := io.ReadFull(c.r, one[:]); err != nil {
				return 0, err // io.EOF: the input ends exactly at the limit
			}
			return 0, errLimit
		}
		if rem := c.limit - c.n; int64(len(p)) > rem {
			p = p[:rem]
		}
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	total := c.n
	c.im.update(func(rep *Report) { rep.Bytes = total })
	return n, err
}
