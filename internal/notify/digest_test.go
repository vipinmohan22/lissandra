package notify

import (
	"context"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

// testDigestMessage is a digest message of n entries with samples of
// sampleLen bytes, as flushLocked builds one.
func testDigestMessage(n, sampleLen int) *Message {
	now := t0.Add(30 * time.Minute)
	dg := &Digest{ID: "dg1", Reason: ReasonDigest, From: t0, To: now, IntervalMinutes: 30, Routes: []string{"dev-slack"},
		Highest: model.SeverityError, Link: "https://lissandra.test/#/deliveries/dg1"}
	for i := 0; i < n; i++ {
		sample := strings.Repeat(fmt.Sprintf("<b>%d</b> & `x` ", i), sampleLen/14+1)[:sampleLen]
		e := DigestEntry{Fingerprint: fmt.Sprintf("fp%03d", i), SourceName: "checkout-api", Category: "database", Severity: model.SeverityError,
			Count: 100 - i, First: t0.Add(2 * time.Minute), Last: t0.Add(29 * time.Minute), Sample: sample, EventID: fmt.Sprint("ev", i),
			Link: fmt.Sprintf("https://lissandra.test/#/events?fingerprint=fp%03d", i)}
		switch i {
		case 0:
			e.Held, e.HeldBy = ReasonNew, ReasonSourceLimit
		case 1:
			e.New = true
		case 2:
			e.Repeats, e.AlertedAt = 10, t0.Add(time.Minute)
		case 3:
			e.Undelivered = 2
		}
		if i < maxDigestListed {
			dg.Entries = append(dg.Entries, e)
		} else {
			dg.OtherPatterns++
			dg.OtherEvents += e.Count
		}
		dg.Events += e.Count
		dg.Patterns++
	}
	m := &Message{ID: dg.ID, Kind: KindDigest, Digest: dg, Routes: dg.Routes, RouteName: "dev-slack", Link: dg.Link, at: now,
		Reason: Reason{ReasonDigest, "Batched every 30 min on \"dev-slack\"."}}
	m.Event = digestEvent(m)
	return m
}

// 3. A buffer keeps 200 entries and counts the rest; a sent digest lists
// 50, in order, and its counts still add up.
func TestDigestCapsAndOrder(t *testing.T) {
	e := newEnv(t, Options{}, &config.Config{
		Notifiers: []config.Notifier{chat("slack")},
		Routes:    []config.Route{smartRoute("r", model.SeverityError, 30, 3600, "slack")},
	})
	events := 0
	for i := 0; i < 300; i++ {
		for k := 0; k <= i%3; k++ {
			e.at(t0.Add(time.Duration(i)*time.Second), e.ev("api", "app", fmt.Sprintf("fp%03d", i), model.SeverityError))
			events++
		}
	}
	b := e.buffer("slack")
	if len(b.Entries) != maxDigestEntries || len(b.Other) != 100 || b.Events != events {
		t.Fatalf("buffer: %d entries, %d other, %d events", len(b.Entries), len(b.Other), b.Events)
	}
	e.flush(t0.Add(time.Hour))
	dg := e.sinks["slack"].digests()[0].Digest
	listed := 0
	for _, en := range dg.Entries {
		listed += en.Count
	}
	if len(dg.Entries) != maxDigestListed || dg.Patterns != 300 || dg.OtherPatterns != 250 || listed+dg.OtherEvents != events || dg.Events != events {
		t.Errorf("digest: %d listed, patterns %d, other %d/%d, events %d of %d", len(dg.Entries), dg.Patterns, dg.OtherPatterns, dg.OtherEvents, dg.Events, events)
	}

	// Held first, then severity, count, last time, fingerprint.
	buf := &digestBuffer{Entries: map[string]*DigestEntry{
		"a": {Fingerprint: "a", Severity: model.SeverityError, Count: 5, Last: t0},
		"b": {Fingerprint: "b", Severity: model.SeverityCritical, Count: 1, Last: t0},
		"c": {Fingerprint: "c", Severity: model.SeverityWarning, Count: 1, Last: t0, Held: ReasonNew},
		"d": {Fingerprint: "d", Severity: model.SeverityError, Count: 5, Last: t0.Add(time.Minute)},
		"e": {Fingerprint: "e", Severity: model.SeverityError, Count: 9, Last: t0},
		"f": {Fingerprint: "f", Severity: model.SeverityError, Count: 5, Last: t0},
	}}
	var order []string
	for _, en := range sortedEntries(buf) {
		order = append(order, en.Fingerprint)
	}
	if got := strings.Join(order, ""); got != "cbedaf" {
		t.Errorf("order = %s", got)
	}
}

// 21. Channel layouts for digests stay within each channel's limits.
func TestDigestPayloads(t *testing.T) {
	var c capture
	srv := c.server(t, http.StatusOK)
	ctx := context.Background()
	big := testDigestMessage(60, sampleBytes)
	for _, n := range []config.Notifier{
		{Type: "slack", Settings: map[string]string{"webhook_url": srv.URL + "/slack", "mention": "<!here>"}},
		{Type: "teams", Settings: map[string]string{"webhook_url": srv.URL + "/teams"}},
		{Type: "discord", Settings: map[string]string{"webhook_url": srv.URL + "/discord"}},
		{Type: "googlechat", Settings: map[string]string{"webhook_url": srv.URL + "/gchat"}},
		{Type: "webhook", Settings: map[string]string{"url": srv.URL + "/hook"}},
		{Type: "webhook", Settings: map[string]string{"url": srv.URL + "/tmpl", "body_template": `{"t": {{ json .Title }}, "s": {{ json .Event.Severity }}, "k": {{ json .Kind }}}`}},
	} {
		s, err := Build(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Send(ctx, big); err != nil {
			t.Fatalf("%s: %v", n.Type, err)
		}
	}
	reqs := c.all()
	if len(reqs) != 6 {
		t.Fatalf("requests = %d", len(reqs))
	}

	// Slack: ≤ 20 blocks, sections ≤ 3000, the count linked, no mention.
	slackBody := reqs[0].body
	if slackBody["text"] != big.Title() {
		t.Errorf("slack text = %v", slackBody["text"])
	}
	blocks := slackBody["attachments"].([]any)[0].(map[string]any)["blocks"].([]any)
	if len(blocks) > 20 {
		t.Errorf("slack blocks = %d", len(blocks))
	}
	for _, b := range blocks {
		bm := b.(map[string]any)
		if bm["type"] == "section" && len(bm["text"].(map[string]any)["text"].(string)) > 3000 {
			t.Error("a slack section is over 3000 characters")
		}
	}
	head := blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	entry := blocks[1].(map[string]any)["text"].(map[string]any)["text"].(string)
	if strings.Contains(head, "<!here>") || !strings.Contains(entry, "<https://lissandra.test/#/events?fingerprint=fp000|×100>") ||
		!strings.Contains(entry, "&lt;b&gt;") || strings.Contains(entry, "<b>") || !strings.Contains(entry, "HELD (new)") {
		t.Errorf("slack: head %q, entry %q", head, entry)
	}
	if !strings.Contains(reqs[0].raw, "Open digest") || !strings.Contains(reqs[0].raw, "+ 45 more problems") {
		t.Error("slack digest without the button or overflow line")
	}

	// Teams: under 28 KB.
	if len(reqs[1].raw) >= 28<<10 || !strings.Contains(reqs[1].raw, "Open digest") {
		t.Errorf("teams payload: %d bytes", len(reqs[1].raw))
	}
	// Discord: description ≤ 4096, embed ≤ 6000, no mentions.
	embed := reqs[2].body["embeds"].([]any)[0].(map[string]any)
	desc := embed["description"].(string)
	total := len(embed["title"].(string)) + len(desc) + len(embed["footer"].(map[string]any)["text"].(string))
	if len(desc) > 4096 || total > 6000 || reqs[2].body["allowed_mentions"] == nil || embed["url"] != big.Link {
		t.Errorf("discord: description %d, embed %d", len(desc), total)
	}
	// Google Chat: ≤ 4000 characters, markup escaped.
	text := reqs[3].body["text"].(string)
	if len(text) > 4000 || strings.Contains(text, "<b>") || !strings.Contains(text, "‹b›") || !strings.Contains(text, "|Open digest>") {
		t.Errorf("google chat: %d chars: %.300s", len(text), text)
	}
	// Webhook: the standard payload carries kind, reason and the digest.
	hook := reqs[4].body
	if hook["kind"] != "digest" || hook["id"] != "dg1" || hook["reason"].(map[string]any)["code"] != ReasonDigest ||
		hook["digest"].(map[string]any)["events"] != float64(big.Digest.Events) || hook["event"].(map[string]any)["severity"] != "error" {
		t.Errorf("webhook payload: %.500s", reqs[4].raw)
	}
	// An alert template written before digests still renders.
	if tm := reqs[5].body; tm["s"] != "error" || tm["k"] != "digest" || !strings.HasPrefix(tm["t"].(string), "[DIGEST] 60 problems") {
		t.Errorf("template: %s", reqs[5].raw)
	}

	// Held criticals: Slack mentions.
	held := testDigestMessage(2, 20)
	held.Digest.CriticalHeld, held.Digest.Held, held.Digest.Reason = 1, 1, ReasonSummary
	blocksHeld := (&slack{s: map[string]string{"mention": "<!here>"}}).digestBlocks(held)
	if h := blocksHeld[0]["text"].(map[string]any)["text"].(string); !strings.HasPrefix(h, "<!here> *[SUMMARY] 1 alert held back · checkout-api*") {
		t.Errorf("summary head: %q", h)
	}
}

func TestDigestText(t *testing.T) {
	m := testDigestMessage(53, 30)
	if got := m.Title(); got != "[DIGEST] 53 problems · "+fmt.Sprint(m.Digest.Events)+" events · highest ERROR" {
		t.Errorf("title = %q", got)
	}
	m.Test = true
	if !strings.HasPrefix(m.Title(), "[TEST] [DIGEST]") {
		t.Errorf("test title = %q", m.Title())
	}
	m.Test = false
	text := m.PlainText()
	for _, want := range []string{
		"ERROR ×100  checkout-api · database  09:02–09:29 UTC  HELD (new)\n",
		"ERROR ×99  checkout-api · database  09:02–09:29 UTC  NEW\n",
		"REPEATS of 09:01\n", "2 UNDELIVERED\n",
		"    https://lissandra.test/#/events?fingerprint=fp000\n",
		"+ 3 more problems (",
		"\nWhy: Batched every 30 min",
		"Open in Lissandra: https://lissandra.test/#/deliveries/dg1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plain text lacks %q", want)
		}
	}
	facts := m.Facts()
	var names []string
	for _, f := range facts {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "Why,Period,Routes,Events,Problems,Highest" || facts[1].Value != "09:00–09:30 UTC" {
		t.Errorf("facts = %+v", facts)
	}
	if ev := m.Event; ev.Severity != model.SeverityError || ev.SourceName != "checkout-api" || ev.Category != "digest" || ev.ID != "dg1" {
		t.Errorf("digest event = %+v", ev)
	}
}

func TestEmailDigest(t *testing.T) {
	e := &email{subjectPrefix: "[Lissandra]", from: &mail.Address{Address: "l@example.com"}, to: []*mail.Address{{Address: "a@example.com"}}}
	m := testDigestMessage(14, 40)
	raw, err := e.compose(m)
	if err != nil {
		t.Fatal(err)
	}
	msg := string(raw)
	subject := fmt.Sprintf("[Lissandra] Digest: 14 problems, %d events (highest error)", m.Digest.Events)
	for _, want := range []string{"X-Lissandra-Kind: digest", "X-Lissandra-Digest: dg1", "X-Lissandra-Severity: error", mime.QEncoding.Encode("utf-8", subject)} {
		if !strings.Contains(msg, want) {
			t.Errorf("email lacks %q", want)
		}
	}
	if strings.Contains(msg, "X-Lissandra-Fingerprint") {
		t.Error("a digest has a fingerprint header")
	}
	_, htmlPart, _ := strings.Cut(msg, "Content-Type: text/html")
	html, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(htmlPart)))
	if n := strings.Count(string(html), "<tr style=\"border-top"); n != 14 || strings.Contains(string(html), "<b>0</b>") {
		t.Errorf("html rows = %d", n)
	}
	_, plainPart, _ := strings.Cut(msg, "Content-Type: text/plain")
	plain, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(plainPart)))
	if !strings.Contains(string(plain), "ERROR ×100  checkout-api · database") {
		t.Errorf("plain part: %.300s", plain)
	}
	m.Digest.Reason, m.Digest.Held = ReasonSummary, 99
	if got := e.subject(m); got != "[Lissandra] Summary: 99 alerts held back (highest error)" {
		t.Errorf("summary subject = %q", got)
	}
	alert := testMessage()
	alert.Reason = Reason{ReasonNew, "New problem."}
	if got := e.subject(alert); !strings.HasPrefix(got, "[Lissandra] [CRITICAL] application · api · new problem: ") {
		t.Errorf("alert subject = %q", got)
	}
	raw, _ = e.compose(alert)
	if !strings.Contains(string(raw), "X-Lissandra-Kind: alert") || !strings.Contains(string(raw), "X-Lissandra-Fingerprint: abc") {
		t.Error("alert headers")
	}
}
