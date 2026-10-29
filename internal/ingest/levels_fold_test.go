package ingest

import (
	"testing"
	"time"

	"github.com/vipinmohan22/lissandra/internal/classify"
	"github.com/vipinmohan22/lissandra/internal/config"
)

func TestNumericLevelNeedsALogRecord(t *testing.T) {
	src := &config.Source{ID: "s", Name: "s", Parse: config.Parse{Format: config.FormatAuto}}
	for body, want := range map[string]string{
		`{"datetime":"2026-10-11 10:15:00","station":"kaub","river":"rhine","level":400,"unit":"cm"}`:             "",
		`{"time":"2026-10-11T10:00:00Z","msg":"tank reading","sensor":"tank-3","level":60,"unit":"percent"}`:      "",
		`{"datetime":"2026-10-11 10:00:00","event":"level_up","player":"steve","level":30}`:                       "",
		`{"level":50,"time":1791712800000,"pid":7,"hostname":"api-1","msg":"boom"}`:                               "50",
		`{"name":"api","hostname":"h","pid":1,"level":40,"msg":"slow","time":"2026-10-11T10:00:00Z","v":0}`:       "40",
		`{"message":"x","level":400,"level_name":"ERROR","channel":"app","datetime":"2026-10-11T10:00:00+00:00"}`: "400",
		`{"version":"1.1","host":"h","short_message":"boom","level":3}`:                                           "3",
		`{"MESSAGE":"started","PRIORITY":"6"}`:                                                                    "6",
		`{"message":"disk almost full","level":4}`:                                                                "4",
		`{"level":"warn","msg":"string levels are kept"}`:                                                         "warn",
		// pino with base: null, an ISO time, no time, or messageKey "message"
		`{"level":60,"time":"2026-10-29T10:00:00.000Z","msg":"cannot continue: config store unavailable, exiting"}`: "60",
		`{"level":50,"time":"2026-10-29T10:00:00.000Z","msg":"charge request to stripe returned 402"}`:              "50",
		`{"level":50,"time":1793268000000,"message":"charge request to stripe returned 402"}`:                       "50",
		`{"level":50,"msg":"charge request to stripe returned 402"}`:                                                "50",
		`{"level":30,"time":1793268000000,"msg":"tank reading","sensor":"t3"}`:                                      "",
		`{"level":"50","message":"request errored","trace_id":"4bf92f35"}`:                                          "50",
		// a game's or a device's level with a message, but not in pino's shape
		`{"time":"2026-10-29T10:00:00Z","msg":"player reached a new level","player":"steve","level":60}`: "",
		`{"event":"level_up","player":"steve","level":50,"message":"steve levelled up"}`:                 "",
		`{"device":"tank-3","msg":"fill level","level":60,"pct":true}`:                                   "",
		`{"time":"2026-10-29T10:00:00Z","msg":"battery status","device":"phone-7","level":10}`:           "",
	} {
		evs, err := Parse([]byte(body), "application/json", src, time.Now())
		if err != nil || len(evs) != 1 || evs[0].Level != want {
			t.Errorf("%s: level %q, want %q (%v)", body, evs[0].Level, want, err)
		}
	}
}

func TestMongoStructuredLevel(t *testing.T) {
	src := &config.Source{ID: "s", Name: "s", Parse: config.Parse{Format: config.FormatAuto}}
	for s, want := range map[string]string{"W": "W", "E": "E", "F": "F", "I": "I", "D2": "D2", "X": "", "D9": ""} {
		body := `{"t":{"$date":"2026-10-11T10:00:00.456+00:00"},"s":"` + s + `","c":"CONTROL","id":22120,"ctx":"initandlisten","msg":"Access control is not enabled"}`
		evs, _ := Parse([]byte(body), "", src, time.Now())
		if len(evs) != 1 || evs[0].Level != want || evs[0].Message != "Access control is not enabled" {
			t.Errorf("s=%s: %+v", s, evs)
		}
	}
}

func TestTextFoldsUnindentedContinuations(t *testing.T) {
	src := &config.Source{ID: "s", Name: "s", Parse: config.Parse{Format: config.FormatText}}
	for name, c := range map[string]struct {
		body string
		n    int
	}{
		"jul":                            {"Oct 11, 2026 10:00:00 AM org.example.Foo run\nSEVERE: Connection pool exhausted\nOct 11, 2026 10:00:01 AM org.example.Foo run\nINFO: ok\n", 2},
		"jul not after a header":         {"started\nSEVERE: Connection pool exhausted\n", 2},
		"go panic":                       {"panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=0x1 addr=0x18 pc=0x441c77]\n\ngoroutine 630 [running]:\nmain.(*CheckoutHandler).ServeHTTP(0xc000123450, {0x9a1b20, 0xc0001c2000}, 0xc0001b4000)\n\t/src/checkout/handler.go:142 +0x1d4\nnet/http.serverHandler.ServeHTTP({0xc0000a6ff0?}, {0x9a1b20?, 0xc0001c2000?}, 0x6?)\n\t/usr/local/go/src/net/http/server.go:3210 +0x8e\ncreated by net/http.(*Server).Serve in goroutine 1\nexit status 2\n", 1},
		"python":                         {"2026-10-29 07:00:14,293 ERROR tasks: job send_receipt id=81292 failed\nTraceback (most recent call last):\n  File \"/app/worker/tasks.py\", line 88, in run\n    result = handler(payload)\nKeyError: 'customer'\n2026-10-29 07:00:15,000 INFO tasks: next\n", 2},
		"python bare":                    {"Traceback (most recent call last):\n  File \"/app/w.py\", line 41, in send\n    email = p[\"email\"]\nKeyError: 'customer_email'\n", 1},
		"python chained":                 {"Traceback (most recent call last):\n  File \"a.py\", line 1, in f\n    g()\nValueError: x\n\nDuring handling of the above exception, another exception occurred:\n\nTraceback (most recent call last):\n  File \"a.py\", line 3, in h\n    f()\nRuntimeError: y\n", 1},
		"python not after info":          {"INFO done\nTraceback (most recent call last):\n  File \"a.py\", line 1, in f\nKeyError: 'x'\n", 2},
		"java header":                    {"2026-10-29 07:00:09.293 ERROR 1 --- [nio-8080-exec-10] o.a.c.c.C.[.[.[/].[dispatcherServlet] : Servlet.service() threw exception with root cause\njava.lang.NullPointerException: Cannot invoke \"x()\"\n\tat com.shop.TaxService.computeTax(TaxService.java:87)\n", 1},
		"exception word is not a header": {"ERROR failed\nException handling is configured\n", 2},
		"two tracebacks":                 {"Traceback (most recent call last):\n  File \"/app/worker.py\", line 42, in process\n    handle(job)\nValueError: bad payload\nTraceback (most recent call last):\n  File \"/app/db.py\", line 5, in connect\n    conn = psycopg2.connect(dsn)\npsycopg2.OperationalError: could not connect to server: Connection refused\n", 2},
		"warning then a crash":           {"2026-10-29 10:00:00,000 WARNING worker: transient failure, retrying\nTraceback (most recent call last):\n  File \"/app/worker.py\", line 10, in run\n    fetch()\nrequests.exceptions.ConnectionError: timeout\nTraceback (most recent call last):\n  File \"/app/main.py\", line 3, in <module>\n    main()\nMemoryError\n", 2},
		"jul with its throwable":         {"Oct 29, 2026 10:00:00 AM com.shop.OrderService place\nSEVERE: Order placement failed\njava.lang.IllegalStateException: ledger closed\n\tat com.shop.OrderService.place(OrderService.java:42)\n\tat com.shop.Api.handle(Api.java:10)\n", 1},
		"jul info then an exception":     {"Oct 29, 2026 10:00:00 AM com.shop.OrderService place\nINFO: placed\nOct 29, 2026 10:00:01 AM com.shop.OrderService place\nSEVERE: failed\njava.lang.IllegalStateException: x\n\tat a.B.c(B.java:1)\n", 2},
	} {
		evs, _ := Parse([]byte(c.body), "text/plain", src, time.Now())
		if len(evs) != c.n {
			t.Errorf("%s: %d events, want %d: %q", name, len(evs), c.n, messages(evs))
		}
	}
}

// A level journald, Docker's GELF driver or a syslog PRI gives a whole
// stream (6 for stdout, 3 for stderr) is not the program's: rules that keep
// a stated level still apply to the text.
func TestStreamDefaultLevelKeepsRules(t *testing.T) {
	src := &config.Source{ID: "s", Name: "s", Parse: config.Parse{Format: config.FormatAuto}}
	c := config.Defaults()
	if err := config.Normalize(c); err != nil {
		t.Fatal(err)
	}
	e := classify.New(c.Rules)
	for _, tc := range []struct{ body, want string }{
		{`{"MESSAGE":"Exception in thread \"main\" java.lang.IllegalStateException: ledger closed\n\tat com.shop.App.main(App.java:10)","PRIORITY":"6","_SYSTEMD_UNIT":"shop.service"}`, "error"},
		{`{"MESSAGE":"java.lang.OutOfMemoryError: Java heap space","PRIORITY":"6","_SYSTEMD_UNIT":"shop.service"}`, "error"},
		{`{"version":"1.1","host":"web1","short_message":"10.0.0.1 - - [29/Oct/2026:10:00:00 +0000] \"GET /api/orders HTTP/1.1\" 502 157 \"-\" \"curl/8.5.0\"","level":6,"_container_name":"nginx"}`, "error"},
		{`{"MESSAGE":"Failed password for invalid user admin from 203.0.113.9 port 52212 ssh2","PRIORITY":"6","SYSLOG_IDENTIFIER":"sshd"}`, "warning"},
		{`{"MESSAGE":"2026-10-29 10:00:00 INFO Exception mapper registered","PRIORITY":"6"}`, "info"}, // the text states its level
	} {
		evs, err := Parse([]byte(tc.body), "application/json", src, time.Now())
		if err != nil || len(evs) != 1 {
			t.Fatalf("%v %d", err, len(evs))
		}
		ev := evs[0]
		e.Classify(&ev)
		if string(ev.Severity) != tc.want {
			t.Errorf("got %s, want %s (level %q, rules %v): %.90s", ev.Severity, tc.want, ev.Level, ev.MatchedRules, ev.Message)
		}
	}
}
