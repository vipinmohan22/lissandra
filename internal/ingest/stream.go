package ingest

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/model"
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

// ErrRecordTooLarge is returned when one JSON value exceeds the limit.
var ErrRecordTooLarge = errors.New("a JSON record is larger than the per-record limit")

// StreamOptions configures Stream.
type StreamOptions struct {
	Format         string         // auto (default), text, ndjson, json or csv; overrides the source's
	FileName       string         // used to recognise .csv files in auto mode
	Location       *time.Location // zone for timestamps without one (default UTC)
	MaxRecordBytes int64          // cap per JSON value in JSON mode: 0 means DefaultMaxRecordBytes, negative no cap
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

	br, err := st.sniff(r)
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
		return nil, err
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

// skipNULPadding discards a leading run of NUL bytes, which logrotate's
// copytruncate leaves when a writer keeps its old file offset. Short runs
// are kept: BOM-less UTF-16BE text starts with a single NUL.
func skipNULPadding(br *bufio.Reader) {
	for {
		peek, _ := br.Peek(64 << 10)
		n := 0
		for n < len(peek) && peek[n] == 0 {
			n++
		}
		if n < 64 {
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
	if strings.HasSuffix(strings.ToLower(st.opts.FileName), ".csv") {
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
	if !bytes.Contains(body[:end], []byte{'\n'}) && len(rest) > 0 && rest[0] == '\n' && len(bytes.TrimSpace(rest)) > 0 {
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
			if errors.Is(err, io.EOF) {
				sp.flush(collect)
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
	paths := wrapperPaths(st.src.Parse.RecordsPath)
	emitted := 0
	fail := func(err error) error {
		if errors.Is(err, ErrRecordTooLarge) {
			return err
		}
		if emitted > 0 {
			return &SyntaxError{Line: max(cr.lineAt(dec.InputOffset()), st.jsonLine), Err: err}
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	defer func() { st.stats.Lines = cr.lineAt(cr.read) }()
	for {
		cr.window = dec.InputOffset()
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fail(err)
		}
		switch tok {
		case json.Delim('['):
			if err := st.streamArray(dec, cr, &emitted); err != nil {
				return fail(err)
			}
		case json.Delim('{'):
			line := cr.lineAt(dec.InputOffset())
			obj, streamed, err := st.walkObject(dec, cr, paths, &emitted)
			if err != nil {
				return fail(err)
			}
			if !streamed && st.err == nil {
				if st.src.Parse.RecordsPath != "" {
					return fmt.Errorf("records path %q not found in the JSON", st.src.Parse.RecordsPath)
				}
				st.record(obj, line)
				emitted++
			}
		default:
			st.record(tok, cr.lineAt(dec.InputOffset()))
			emitted++
		}
		if st.err != nil {
			return st.err
		}
	}
}

// streamArray emits each element of an array whose '[' was consumed.
func (st *streamer) streamArray(dec *json.Decoder, cr *jsonCounter, emitted *int) error {
	for dec.More() {
		line := cr.lineAt(dec.InputOffset()) + leadingNewlines(dec)
		st.jsonLine = line
		cr.window = dec.InputOffset()
		var v any
		if err := dec.Decode(&v); err != nil {
			return err
		}
		st.record(v, line)
		*emitted++
		if st.err != nil {
			return nil
		}
	}
	_, err := dec.Token() // closing ']'
	return err
}

func wrapperPaths(recordsPath string) [][]string {
	if recordsPath != "" {
		return [][]string{strings.Split(recordsPath, ".")}
	}
	out := make([][]string, len(wrapperKeys))
	for i, k := range wrapperKeys {
		out[i] = strings.Split(k, ".")
	}
	return out
}

// walkObject reads the rest of an object whose '{' was consumed. When a key
// leads along one of paths to an array, the array's elements are emitted
// as records (streamed=true); otherwise the object is returned whole.
func (st *streamer) walkObject(dec *json.Decoder, cr *jsonCounter, paths [][]string, emitted *int) (map[string]any, bool, error) {
	obj := map[string]any{}
	streamed := false
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false, err
		}
		key, _ := t.(string)
		var sub [][]string
		if !streamed {
			for _, p := range paths {
				if p[0] == key {
					sub = append(sub, p[1:])
				}
			}
		}
		cr.window = dec.InputOffset()
		if len(sub) == 0 {
			var v any
			if err := dec.Decode(&v); err != nil {
				return nil, false, err
			}
			obj[key] = v
			continue
		}
		atTarget := false
		var deeper [][]string
		for _, p := range sub {
			if len(p) == 0 {
				atTarget = true
			} else {
				deeper = append(deeper, p)
			}
		}
		vt, err := dec.Token()
		if err != nil {
			return nil, false, err
		}
		switch vt {
		case json.Delim('['):
			if !atTarget {
				arr, err := decodeRestOfArray(dec, cr)
				if err != nil {
					return nil, false, err
				}
				obj[key] = arr
				continue
			}
			if err := st.streamArray(dec, cr, emitted); err != nil {
				return nil, false, err
			}
			if st.err != nil {
				return nil, true, nil
			}
			streamed = true
		case json.Delim('{'):
			inner, innerStreamed, err := st.walkObject(dec, cr, deeper, emitted)
			if err != nil {
				return nil, false, err
			}
			if innerStreamed {
				streamed = true
				if st.err != nil {
					return nil, true, nil
				}
			} else {
				obj[key] = inner
			}
		default:
			obj[key] = vt
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, false, err
	}
	return obj, streamed, nil
}

// decodeRestOfArray decodes array elements after its '[' was consumed.
func decodeRestOfArray(dec *json.Decoder, cr *jsonCounter) ([]any, error) {
	arr := []any{}
	for dec.More() {
		cr.window = dec.InputOffset()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		arr = append(arr, v)
	}
	_, err := dec.Token()
	return arr, err
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
}

func (c *jsonCounter) Read(p []byte) (int, error) {
	if c.max > 0 && c.read-c.window > c.max {
		return 0, ErrRecordTooLarge
	}
	n, err := c.r.Read(p)
	for i, b := range p[:n] {
		if b == '\n' {
			c.nls = append(c.nls, c.read+int64(i))
		}
	}
	c.read += int64(n)
	return n, err
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
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("invalid CSV header: %w", err)
	}
	header = append([]string(nil), header...)
	for {
		row, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		line, _ := cr.FieldPos(0)
		if err != nil {
			return &SyntaxError{Line: line, Err: err}
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
