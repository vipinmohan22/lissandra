package ingest

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// FormatCSV parses comma-separated exports (Kibana, CloudWatch Insights).
// It is available for file analysis only.
const FormatCSV = "csv"

// Stream limits.
const (
	peekSize = 1 << 20 // read buffer, and how far auto-detection looks
	// MaxLineBytes caps one physical line; longer lines are cut (and can no
	// longer decode as JSON). Messages are separately capped at MaxMessageBytes.
	MaxLineBytes = peekSize
	// DefaultMaxRecordBytes caps one JSON value in JSON mode.
	DefaultMaxRecordBytes = 16 << 20
	// maxContinuationLines caps how many lines fold into one record.
	maxContinuationLines = 1000
)

// UnsupportedError reports input Stream refuses to parse (archives,
// binary data).
type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string { return e.Reason }

// SyntaxError reports malformed input after some records were read; the
// records before it are valid.
type SyntaxError struct {
	Line int
	Err  error
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("invalid input near line %d: %v", e.Line, e.Err)
}
func (e *SyntaxError) Unwrap() error { return e.Err }

// ErrRecordTooLarge is returned when one JSON value or CSV row exceeds the
// limit.
var ErrRecordTooLarge = errors.New("a single record is larger than the per-record limit")

// StreamOptions configures Stream.
type StreamOptions struct {
	Format         string         // auto (default), text, ndjson, json or csv; overrides the source's
	FileName       string         // used to recognise .csv files in auto mode
	Location       *time.Location // zone for timestamps without one (default UTC)
	MaxRecordBytes int64          // cap per JSON value or CSV row: 0 means DefaultMaxRecordBytes, negative no cap
}

// StreamStats describes how the input was parsed.
type StreamStats struct {
	Mode     string `json:"mode"`     // json, ndjson, csv, text (indent folding) or text-anchored
	Encoding string `json:"encoding"` // utf-8, utf-8-bom, utf-16le or utf-16be
	Lines    int    `json:"lines"`

	LongLines    int `json:"long_lines,omitempty"`                 // lines cut at MaxLineBytes
	FoldedLines  int `json:"folded_lines,omitempty"`               // continuation lines joined into records
	DroppedLines int `json:"dropped_continuation_lines,omitempty"` // beyond maxContinuationLines
	JSONErrors   int `json:"json_line_errors,omitempty"`           // '{' lines that were not valid JSON

	TimeParsed   int64 `json:"timestamps_parsed"`
	TimeInferred int64 `json:"timestamps_inferred"` // copied from the previous record
	TimeMissing  int64 `json:"timestamps_missing"`  // before any timestamp in the file; left empty
}

// streamer holds the state of one Stream call.
type streamer struct {
	src    *config.Source
	now    time.Time
	opts   StreamOptions
	tp     timeParser
	emit   func(model.Event) error
	stats  StreamStats
	lastTS time.Time
	err    error // first error from emit

	jsonLine int // line where the JSON value being decoded starts
}

// Stream reads log records from r incrementally and calls emit for each
// event in order. Memory stays bounded: lines, JSON values and CSV rows are
// processed one at a time. Records without a timestamp inherit the previous
// record's (TimeSource "inferred"); records before any timestamp have none
// (zero Timestamp, TimeSource "none"). An error from emit stops the stream
// and is returned.
func Stream(r io.Reader, src *config.Source, now time.Time, opts StreamOptions, emit func(model.Event) error) (StreamStats, error) {
	if opts.MaxRecordBytes == 0 {
		opts.MaxRecordBytes = DefaultMaxRecordBytes
	}
	st := &streamer{src: src, now: now, opts: opts, emit: emit,
		tp: timeParser{loc: opts.Location, ref: now, extended: true}}

	br, err := st.sniff(&stickyReader{r: r})
	if err != nil {
		return st.stats, err
	}
	format := opts.Format
	if format == "" {
		format = src.Parse.Format
	}
	switch format {
	case config.FormatJSON:
		err = st.json(br)
	case config.FormatNDJSON:
		st.stats.Mode = "ndjson"
		err = st.lines(br, lineSplitter{decode: true})
	case config.FormatText:
		err = st.lines(br, st.textSplitter(br, false))
	case FormatCSV:
		err = st.csv(br)
	default:
		err = st.auto(br)
	}
	if err == nil {
		err = st.err
	}
	return st.stats, err
}

// sniff detects the encoding (transcoding UTF-16) and rejects archives and
// binary data. It returns a reader positioned after any BOM.
func (st *streamer) sniff(r io.Reader) (*bufio.Reader, error) {
	br := bufio.NewReaderSize(r, peekSize)
	head, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		if len(head) == 0 {
			return nil, err
		}
		// Short input that then failed: what arrived is read first, and the
		// sticky error comes back after it.
	}
	st.stats.Encoding = "utf-8"
	switch {
	case bytes.HasPrefix(head, []byte("\xef\xbb\xbf")):
		st.stats.Encoding = "utf-8-bom"
		br.Discard(3)
	case bytes.HasPrefix(head, []byte("\xff\xfe")):
		st.stats.Encoding = "utf-16le"
		br.Discard(2)
		br = bufio.NewReaderSize(newUTF16Reader(br, false), peekSize)
	case bytes.HasPrefix(head, []byte("\xfe\xff")):
		st.stats.Encoding = "utf-16be"
		br.Discard(2)
		br = bufio.NewReaderSize(newUTF16Reader(br, true), peekSize)
	default:
		skipNULPadding(br)
		head, _ = br.Peek(512)
		if le, ok := looksUTF16(head); ok {
			if le {
				st.stats.Encoding = "utf-16le"
			} else {
				st.stats.Encoding = "utf-16be"
			}
			br = bufio.NewReaderSize(newUTF16Reader(br, !le), peekSize)
		}
	}
	if err := checkSupported(br); err != nil {
		return nil, err
	}
	return br, nil
}

// stickyReader repeats the first error from r. bufio's Peek hands an error
// to its caller once and forgets it; sniffing and auto-detection peek and
// ignore errors, so without this a stop such as a size limit could be lost.
type stickyReader struct {
	r   io.Reader
	err error
}

func (s *stickyReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	s.err = err
	return n, err
}

// skipNULPadding discards a leading run of NUL bytes, which logrotate's
// copytruncate leaves when a writer keeps its old file offset. Short runs
// are kept: BOM-less UTF-16BE text starts with a single NUL.
func skipNULPadding(br *bufio.Reader) {
	for skipped := false; ; skipped = true {
		peek, _ := br.Peek(64 << 10)
		n := 0
		for n < len(peek) && peek[n] == 0 {
			n++
		}
		if n == 0 || n < 64 && !skipped {
			return
		}
		br.Discard(n)
		if n < len(peek) {
			return
		}
	}
}

var archiveMagic = []struct {
	magic []byte
	name  string
}{
	{[]byte("PK\x03\x04"), "a zip archive"},
	{[]byte("\xfd7zXZ\x00"), "an xz archive"},
	{[]byte("BZh"), "a bzip2 file"},
	{[]byte("\x28\xb5\x2f\xfd"), "a zstd file"},
	{[]byte("7z\xbc\xaf\x27\x1c"), "a 7-zip archive"},
	{[]byte("\x1f\x8b"), "a gzip file inside a gzip file"},
}

func checkSupported(br *bufio.Reader) error {
	head, _ := br.Peek(8192)
	for _, a := range archiveMagic {
		if bytes.HasPrefix(head, a.magic) {
			return &UnsupportedError{"this looks like " + a.name + "; extract the log file first (gzip is supported)"}
		}
	}
	if len(head) > 262 && bytes.Equal(head[257:262], []byte("ustar")) {
		return &UnsupportedError{"this looks like a tar archive; extract the log file first"}
	}
	if nul := bytes.Count(head, []byte{0}); len(head) > 0 && nul*100 > len(head) {
		return &UnsupportedError{"this looks like a binary file, not a text log"}
	}
	return nil
}

// looksUTF16 detects BOM-less UTF-16 by the NUL bytes in alternate positions
// that ASCII text produces.
func looksUTF16(head []byte) (littleEndian, ok bool) {
	if len(head) < 16 {
		return false, false
	}
	var even, odd int
	for i, b := range head {
		if b == 0 {
			if i%2 == 0 {
				even++
			} else {
				odd++
			}
		}
	}
	half := len(head) / 2
	switch {
	case odd > half*8/10 && even < half/10:
		return true, true
	case even > half*8/10 && odd < half/10:
		return false, true
	}
	return false, false
}

// auto picks a mode from the first megabyte of input. It never fails on
// content: anything that isn't clearly JSON is read as text lines.
func (st *streamer) auto(br *bufio.Reader) error {
	if name := strings.TrimSuffix(strings.ToLower(st.opts.FileName), ".gz"); strings.HasSuffix(name, ".csv") {
		return st.csv(br)
	}
	peek, _ := br.Peek(peekSize)
	body := bytes.TrimLeft(peek, " \t\r\n")
	if len(body) > 0 && (body[0] == '{' || body[0] == '[') {
		switch probeJSON(body, len(peek) < peekSize) {
		case "json":
			return st.json(br)
		case "ndjson":
			st.stats.Mode = "ndjson"
			return st.lines(br, lineSplitter{decode: true, objectsOnly: true, fold: true, maxCont: maxContinuationLines})
		}
	}
	return st.lines(br, st.textSplitter(br, true))
}

// probeJSON classifies input starting with '{' or '[': "json" for a JSON
// document or a stream of multi-line values, "ndjson" when the first value
// is one line followed by more lines, and "" when it isn't JSON.
func probeJSON(body []byte, atEOF bool) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if body[0] == '[' {
		if _, err := dec.Token(); err != nil {
			return ""
		}
		if !dec.More() {
			return "json" // empty array
		}
		// Records are objects (or arrays). Anything else, such as the
		// "[    0.000000]" of dmesg or "[2026-10-11 …]", is text.
		if c := body[dec.InputOffset()]; c != '{' && c != '[' {
			return ""
		}
	}
	var first json.RawMessage
	err := dec.Decode(&first)
	var syn *json.SyntaxError
	switch {
	case errors.As(err, &syn):
		return ""
	case err != nil:
		if !atEOF && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) {
			return "json" // the first value is larger than the peek window
		}
		return ""
	}
	if body[0] == '[' {
		// A number can decode from text such as "[2026-10-11 …] ERROR",
		// so require valid JSON after the first element too.
		if _, err := dec.Token(); err != nil && (errors.As(err, &syn) || atEOF) {
			return ""
		}
		return "json"
	}
	end := int(dec.InputOffset())
	rest := bytes.TrimLeft(body[end:], " \t\r")
	next := bytes.TrimSpace(rest)
	if len(next) > 0 && next[0] != '{' && next[0] != '[' {
		// Something other than JSON follows the first value, such as text
		// log lines after a JSON banner: read it all as text.
		return ""
	}
	if !bytes.Contains(body[:end], []byte{'\n'}) && len(rest) > 0 && rest[0] == '\n' && len(next) > 0 {
		return "ndjson"
	}
	return "json"
}

// textSplitter chooses between indent folding and timestamp-anchored
// grouping by sampling the start of the input: anchored grouping is used
// when most would-be record starts begin with a timestamp.
func (st *streamer) textSplitter(br *bufio.Reader, decodeJSON bool) lineSplitter {
	sp := lineSplitter{fold: true, decode: decodeJSON, objectsOnly: true, maxCont: maxContinuationLines}
	peek, _ := br.Peek(peekSize)
	starts, stamped := 0, 0
	for i, line := range strings.Split(string(peek), "\n") {
		if i >= 400 {
			break
		}
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || continuation.MatchString(line) {
			continue
		}
		starts++
		if st.tp.startsWithTime(line) {
			stamped++
		}
	}
	st.stats.Mode = "text"
	if stamped >= 3 && stamped*10 >= starts*6 {
		st.stats.Mode = "text-anchored"
		sp.startsRecord = func(line string) bool {
			if decodeJSON && strings.HasPrefix(strings.TrimSpace(line), "{") {
				return true // a JSON line is always its own record
			}
			return st.tp.startsWithTime(line)
		}
	}
	return sp
}

// lines processes line-oriented input.
func (st *streamer) lines(br *bufio.Reader, sp lineSplitter) error {
	collect := func(rec any, line int) { st.record(rec, line) }
	defer func() {
		st.stats.Lines = sp.line
		st.stats.FoldedLines, st.stats.DroppedLines, st.stats.JSONErrors = sp.folded, sp.dropped, sp.jsonErrors
	}()
	for {
		line, long, err := readLine(br)
		if long {
			st.stats.LongLines++
		}
		if len(line) > 0 || err == nil {
			sp.push(line, collect)
		}
		if st.err != nil {
			return st.err
		}
		if err != nil {
			// Keep the record in progress: its lines were read in full
			// even when the input then failed (size limit, truncation).
			sp.flush(collect)
			if errors.Is(err, io.EOF) || st.err != nil {
				return st.err
			}
			return err
		}
	}
}

// readLine returns the next line without its newline. Lines longer than
// MaxLineBytes are cut there (long=true) and the rest is discarded. At end
// of input it returns the final partial line, if any, with io.EOF.
func readLine(br *bufio.Reader) (line string, long bool, err error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if room := MaxLineBytes - len(buf); room > 0 {
			if len(chunk) > room {
				buf = append(buf, chunk[:room]...)
				long = true
			} else {
				buf = append(buf, chunk...)
			}
		} else if len(chunk) > 0 {
			long = true
		}
		switch {
		case err == nil:
			return string(bytes.TrimSuffix(buf, []byte{'\n'})), long, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return string(bytes.TrimSuffix(buf, []byte{'\n'})), long, err
		}
	}
}

// record finalizes one record and emits it.
func (st *streamer) record(rec any, line int) {
	if st.err != nil {
		return
	}
	ev, hasTime := toEvent(rec, st.src.Parse, st.now, st.tp)
	if ev.Message == "" {
		return
	}
	switch {
	case hasTime:
		st.lastTS = ev.Timestamp
		st.stats.TimeParsed++
	case !st.lastTS.IsZero():
		ev.Timestamp, ev.TimeSource = st.lastTS, model.TimeInferred
		st.stats.TimeInferred++
	default:
		ev.Timestamp, ev.TimeSource = time.Time{}, model.TimeNone
		st.stats.TimeMissing++
	}
	stamp(&ev, st.src, st.now)
	ev.Line = line
	st.err = st.emit(ev)
}

// json decodes a stream of JSON values: top-level arrays are read one
// element at a time; objects holding a records array (records_path, or a
// known wrapper key such as "hits.hits") have that array streamed; any
// other value is one record. Successive top-level values (NDJSON,
// concatenated pretty-printed objects) are all read.
func (st *streamer) json(br *bufio.Reader) error {
	st.stats.Mode = "json"
	cr := &jsonCounter{r: br, max: st.opts.MaxRecordBytes}
	dec := json.NewDecoder(cr)
	dec.UseNumber()
	w := &jsonWalker{st: st, dec: dec, cr: cr, targets: recordTargets(st.src.Parse.RecordsPath)}
	fail := func(err error) error {
		switch {
		case st.err != nil:
			return st.err
		case errors.Is(err, ErrRecordTooLarge), errors.Is(err, errRecordsPath):
			return err
		case cr.rerr != nil:
			return err // reading failed (size limit, truncated gzip, …), not the JSON
		case w.emitted > 0:
			return &SyntaxError{Line: max(cr.lineAt(dec.InputOffset()), st.jsonLine), Err: err}
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	defer func() {
		st.stats.Lines = cr.lineAt(cr.read)
		if cr.endsInNewline {
			st.stats.Lines-- // count lines as text mode does
		}
	}()
	for {
		cr.window = dec.InputOffset()
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err == nil {
			switch tok {
			case json.Delim('['):
				err = w.streamArray()
			case json.Delim('{'):
				err = w.object(cr.lineAt(dec.InputOffset()))
			default:
				w.emit(tok, cr.lineAt(dec.InputOffset()))
			}
		}
		if err != nil {
			return fail(err)
		}
		if st.err != nil {
			return st.err
		}
	}
}

var errRecordsPath = errors.New("records path not found")

// candidateBytes is how much of a records array is held back while a
// preferred wrapper key may still follow it in the same object. A larger
// array is taken to be the records and streamed.
const candidateBytes = 256 << 10

// recordTarget is a path, relative to the current value, to a records
// array. rank orders wrapper keys as HTTP ingest does: lower wins.
type recordTarget struct {
	path string
	rank int
}

func recordTargets(recordsPath string) []recordTarget {
	if recordsPath != "" {
		return []recordTarget{{recordsPath, 0}}
	}
	out := make([]recordTarget, len(wrapperKeys))
	for i, k := range wrapperKeys {
		out[i] = recordTarget{k, i}
	}
	return out
}

type jsonElem struct {
	v    any
	line int
}

// jsonWalker finds the records inside one top-level object, choosing among
// wrapper keys the way Records does for HTTP ingest while streaming large
// arrays instead of holding them.
type jsonWalker struct {
	st      *streamer
	dec     *json.Decoder
	cr      *jsonCounter
	targets []recordTarget
	emitted int

	// per top-level object
	found     bool       // a records target was seen; the envelope is not needed
	committed bool       // a records array is being or was streamed
	best      []jsonElem // records of the preferred target held so far
	bestRank  int
	hasBest   bool
}

func (w *jsonWalker) emit(v any, line int) {
	w.st.record(v, line)
	w.emitted++
}

// object reads the rest of a top-level object whose '{' was consumed and
// emits its records, or the object itself when it holds none.
func (w *jsonWalker) object(line int) error {
	w.found, w.committed, w.best, w.hasBest = false, false, nil, false
	obj, err := w.walkObject(w.targets)
	if err != nil {
		return err
	}
	switch {
	case w.committed:
	case w.hasBest:
		for _, e := range w.best {
			if w.emit(e.v, e.line); w.st.err != nil {
				break
			}
		}
		w.best = nil
	case w.st.src.Parse.RecordsPath != "":
		return fmt.Errorf("%w: %q is not in the JSON", errRecordsPath, w.st.src.Parse.RecordsPath)
	default:
		w.emit(obj, line)
	}
	return nil
}

// streamArray emits each element of a top-level array whose '[' was
// consumed as a record.
func (w *jsonWalker) streamArray() error {
	for w.dec.More() {
		v, line, err := w.element()
		if err != nil {
			return err
		}
		if w.emit(v, line); w.st.err != nil {
			return w.st.err
		}
	}
	_, err := w.dec.Token() // closing ']'
	return err
}

// element decodes one array element that is a record of its own: it gets
// its own size budget and the line where it starts.
func (w *jsonWalker) element() (any, int, error) {
	w.cr.window = w.dec.InputOffset()
	w.st.jsonLine = w.cr.lineAt(w.dec.InputOffset()) + leadingNewlines(w.dec)
	var raw json.RawMessage
	if err := w.dec.Decode(&raw); err != nil {
		return nil, 0, err
	}
	line := w.cr.lineAt(w.dec.InputOffset() - int64(len(raw)))
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	err := d.Decode(&v)
	return v, line, err
}

// walkObject reads the rest of an object whose '{' was consumed, following
// keys that lead to targets. The object is returned for use as a record
// when no target is found; values are skipped once one is.
func (w *jsonWalker) walkObject(targets []recordTarget) (map[string]any, error) {
	obj := map[string]any{}
	for w.dec.More() {
		t, err := w.dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var sub []recordTarget
		if !w.committed {
			for _, tg := range targets {
				if tg.path == key {
					sub = append(sub, recordTarget{"", tg.rank})
				} else if strings.HasPrefix(tg.path, key+".") {
					sub = append(sub, recordTarget{tg.path[len(key)+1:], tg.rank})
				}
			}
		}
		v, err := w.value(sub)
		if err != nil {
			return nil, err
		}
		if !w.found {
			obj[key] = v
		}
	}
	_, err := w.dec.Token() // closing '}'
	return obj, err
}

// walkArray reads the rest of an array whose '[' was consumed, following
// numeric path segments.
func (w *jsonWalker) walkArray(targets []recordTarget) ([]any, error) {
	arr := []any{}
	for i := 0; w.dec.More(); i++ {
		idx := strconv.Itoa(i)
		var sub []recordTarget
		if !w.committed {
			for _, tg := range targets {
				if tg.path == idx {
					sub = append(sub, recordTarget{"", tg.rank})
				} else if strings.HasPrefix(tg.path, idx+".") {
					sub = append(sub, recordTarget{tg.path[len(idx)+1:], tg.rank})
				}
			}
		}
		v, err := w.value(sub)
		if err != nil {
			return nil, err
		}
		if !w.found {
			arr = append(arr, v)
		}
	}
	_, err := w.dec.Token() // closing ']'
	return arr, err
}

// value reads the next value, which leads to targets (possibly none).
func (w *jsonWalker) value(targets []recordTarget) (any, error) {
	if len(targets) == 0 {
		if w.found {
			return nil, w.skip()
		}
		var v any
		err := w.dec.Decode(&v)
		return v, err
	}
	rank, atTarget := 0, false
	var deeper []recordTarget
	for _, tg := range targets {
		switch {
		case tg.path != "":
			deeper = append(deeper, tg)
		case !atTarget || tg.rank < rank:
			rank, atTarget = tg.rank, true
		}
	}
	tok, err := w.dec.Token()
	if err != nil {
		return nil, err
	}
	switch {
	case tok == json.Delim('[') && atTarget:
		return nil, w.recordsArray(rank)
	case tok == json.Delim('['):
		return w.walkArray(deeper)
	case tok == json.Delim('{') && len(deeper) > 0:
		return w.walkObject(deeper)
	case atTarget && w.st.src.Parse.RecordsPath != "":
		// records_path naming a single object or value: one record, or
		// none for null, as for HTTP ingest.
		w.found = true
		line := w.cr.lineAt(w.dec.InputOffset())
		v, err := w.rest(tok)
		if err == nil && !w.hasBest {
			w.best, w.bestRank, w.hasBest = nil, rank, true
			if v != nil {
				w.best = []jsonElem{{v, line}}
			}
		}
		return nil, err
	case w.found:
		return nil, w.cr.exclude(w.dec, func() error { return w.skipRest(tok) })
	}
	return w.rest(tok)
}

// recordsArray handles a target array whose '[' was consumed: small arrays
// are held as candidates, the first large one is streamed.
func (w *jsonWalker) recordsArray(rank int) error {
	w.found = true
	if w.hasBest && w.bestRank <= rank {
		return w.cr.exclude(w.dec, func() error { return w.skipRest(json.Delim('[')) })
	}
	start := w.dec.InputOffset()
	return w.cr.exclude(w.dec, func() error {
		held := []jsonElem{}
		streaming := false
		for w.dec.More() {
			v, line, err := w.element()
			if err != nil {
				return err
			}
			if streaming {
				if w.emit(v, line); w.st.err != nil {
					return w.st.err
				}
				continue
			}
			held = append(held, jsonElem{v, line})
			if w.dec.InputOffset()-start > candidateBytes {
				// Too large to hold back: these are the records.
				streaming, w.committed, w.best, w.hasBest = true, true, nil, false
				for _, e := range held {
					if w.emit(e.v, e.line); w.st.err != nil {
						return w.st.err
					}
				}
				held = nil
			}
		}
		if !streaming {
			w.best, w.bestRank, w.hasBest = held, rank, true
		}
		_, err := w.dec.Token() // closing ']'
		return err
	})
}

// rest decodes the rest of a value whose first token was consumed.
func (w *jsonWalker) rest(tok json.Token) (any, error) {
	switch tok {
	case json.Delim('{'):
		obj := map[string]any{}
		for w.dec.More() {
			k, err := w.dec.Token()
			if err != nil {
				return nil, err
			}
			var v any
			if err := w.dec.Decode(&v); err != nil {
				return nil, err
			}
			key, _ := k.(string)
			obj[key] = v
		}
		_, err := w.dec.Token()
		return obj, err
	case json.Delim('['):
		arr := []any{}
		for w.dec.More() {
			var v any
			if err := w.dec.Decode(&v); err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := w.dec.Token()
		return arr, err
	}
	return tok, nil
}

// skip discards the next value without holding it in memory.
func (w *jsonWalker) skip() error {
	return w.cr.exclude(w.dec, func() error {
		w.cr.window = w.dec.InputOffset()
		tok, err := w.dec.Token()
		if err != nil {
			return err
		}
		return w.skipRest(tok)
	})
}

// skipRest discards the rest of a value whose first token was consumed.
// Only one token is held at a time.
func (w *jsonWalker) skipRest(tok json.Token) error {
	depth := 0
	for {
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
		w.cr.window = w.dec.InputOffset()
		var err error
		if tok, err = w.dec.Token(); err != nil {
			return err
		}
	}
}

// leadingNewlines counts newlines in the whitespace (and commas) before the
// decoder's next value, so line numbers point at the value itself.
func leadingNewlines(dec *json.Decoder) int {
	buf, _ := io.ReadAll(io.LimitReader(dec.Buffered(), 4096))
	n := 0
	for _, b := range buf {
		switch b {
		case '\n':
			n++
		case ' ', '\t', '\r', ',':
		default:
			return n
		}
	}
	return n
}

// jsonCounter feeds the JSON decoder, tracking newline positions so byte
// offsets can be mapped to line numbers, and stopping a single value from
// growing past max bytes.
type jsonCounter struct {
	r      io.Reader
	max    int64
	read   int64   // bytes handed to the decoder
	window int64   // offset where the current value started
	nls    []int64 // offsets of newlines not yet passed by lineAt
	passed int     // newlines before the last lineAt offset

	endsInNewline bool
	rerr          error // error from r other than io.EOF
}

func (c *jsonCounter) Read(p []byte) (int, error) {
	if c.max > 0 && c.read-c.window > c.max {
		return 0, ErrRecordTooLarge
	}
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF {
		c.rerr = err
	}
	for i, b := range p[:n] {
		if b == '\n' {
			c.nls = append(c.nls, c.read+int64(i))
		}
	}
	c.read += int64(n)
	if n > 0 {
		c.endsInNewline = p[n-1] == '\n'
	}
	return n, err
}

// exclude runs f over a span of input that does not count toward the size
// of the record being read: the enclosing object's budget is restored
// afterwards.
func (c *jsonCounter) exclude(dec *json.Decoder, f func() error) error {
	used := dec.InputOffset() - c.window
	err := f()
	c.window = dec.InputOffset() - used
	return err
}

// lineAt returns the 1-based line containing byte offset off. Offsets must
// not decrease between calls.
func (c *jsonCounter) lineAt(off int64) int {
	i := 0
	for i < len(c.nls) && c.nls[i] < off {
		i++
	}
	c.passed += i
	c.nls = c.nls[i:]
	return c.passed + 1
}

// csv reads a CSV export with a header row; each row becomes a record keyed
// by the header names.
func (st *streamer) csv(br *bufio.Reader) error {
	st.stats.Mode = "csv"
	cc := &csvCounter{r: br, max: st.opts.MaxRecordBytes}
	cr := csv.NewReader(cc)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		if cc.rerr != nil {
			return err
		}
		return fmt.Errorf("invalid CSV header: %w", err)
	}
	header = append([]string(nil), header...)
	for {
		cc.window = cr.InputOffset()
		row, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		line, _ := cr.FieldPos(0)
		switch {
		case errors.Is(err, ErrRecordTooLarge), err != nil && cc.rerr != nil:
			return err
		case err != nil:
			return &SyntaxError{Line: line, Err: err}
		}
		// With lazy quotes, a field that opens a quote and never closes it
		// runs on through the rest of the file. Stop rather than merge rows.
		nl := 0
		for _, v := range row {
			nl += strings.Count(v, "\n")
		}
		if nl > maxContinuationLines {
			return &SyntaxError{Line: line, Err: fmt.Errorf("a quoted field spans more than %d lines; check for an unbalanced quote", maxContinuationLines)}
		}
		rec := make(map[string]any, len(header))
		for i, v := range row {
			if i < len(header) && v != "" {
				rec[header[i]] = v
			}
		}
		st.record(rec, line)
		st.stats.Lines = line
		if st.err != nil {
			return st.err
		}
	}
}

// csvCounter stops a single CSV record from growing past max bytes.
type csvCounter struct {
	r      io.Reader
	max    int64
	read   int64
	window int64 // offset where the current record started
	rerr   error // error from r other than io.EOF
}

func (c *csvCounter) Read(p []byte) (int, error) {
	if c.max > 0 && c.read-c.window > c.max {
		return 0, ErrRecordTooLarge
	}
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF {
		c.rerr = err
	}
	c.read += int64(n)
	return n, err
}

// stamp sets the identity fields every event carries.
func stamp(ev *model.Event, src *config.Source, now time.Time) {
	ev.ID = model.NewID()
	ev.SourceID = src.ID
	ev.SourceName = src.Name
	ev.ReceivedAt = now
	for _, t := range src.Tags {
		ev.AddTag(t)
	}
}

// finalize turns a record into an event for src (HTTP ingest). ok is false
// for records with no message.
func finalize(rec any, src *config.Source, now time.Time) (model.Event, bool) {
	ev, _ := toEvent(rec, src.Parse, now, timeParser{})
	if ev.Message == "" {
		return ev, false
	}
	stamp(&ev, src, now)
	return ev, true
}
