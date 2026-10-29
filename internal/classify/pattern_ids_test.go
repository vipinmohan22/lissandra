package classify

import (
	"strings"
	"testing"

	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/model"
)

func TestPatternGeneratedIDs(t *testing.T) {
	same := [][2]string{
		// base62 request ids, with and without digits
		{"[req_e3LK2IIpMb] could not get JDBC connection", "[req_QEPeJPPoTB] could not get JDBC connection"},
		{"[req_7Hq2xK9p] could not get JDBC connection", "[req_lwPqnqRTGs] could not get JDBC connection"},
		// Go's pc/addr in a panic's signal line
		{"[signal SIGSEGV: segmentation violation code=0x1 addr=0x18 pc=0x441c77]", "[signal SIGSEGV: segmentation violation code=0x2 addr=0x0 pc=0x4ad370]"},
		// pod names
		{"Container web in pod web-5b7c9d-x2k4p was OOMKilled", "Container web in pod web-7d4f8c-q9w2z was OOMKilled"},
		// mail addresses, accounts
		{"login failed for alice@example.com: invalid password", "login failed for bob.smith@example.org: invalid password"},
		{"sshd[1]: Failed password for invalid user ubuntu from 203.0.113.50 port 49067 ssh2", "sshd[2]: Failed password for invalid user oracle from 203.0.113.51 port 1 ssh2"},
		{"pam_unix(sudo:auth): authentication failure; logname=alice uid=1000 euid=0 ruser=alice rhost=  user=alice", "pam_unix(sudo:auth): authentication failure; logname=bob uid=1001 euid=0 ruser=bob rhost=  user=bob"},
		{`password authentication failed for user "app"`, `password authentication failed for user "reporting"`},
		// id-like key values
		{"job done tid=2kqj jid=b4a577edbccf1d805744efa9", "job done tid=9xr3 jid=0c1d2e3f4a5b6c7d8e9f0a1b"},
		{`{"msg":"done","request_id":"ab12cd34gh"}`, `{"msg":"done","request_id":"zz98yy76xx"}`},
		{"request_id=QEPeJPPoTB done", "request_id=XpPrOKBiZj done"},
		// a variable part of another length before the 400-byte cut
		{"ERROR [nio-8080-exec-10] " + strings.Repeat("x ", 200) + "tail", "ERROR [nio-8080-exec-5] " + strings.Repeat("x ", 200) + "tail"},
	}
	for _, p := range same {
		if a, b := Pattern(p[0]), Pattern(p[1]); a != b {
			t.Errorf("want one pattern:\n  %q -> %q\n  %q -> %q", p[0], a, p[1], b)
		}
	}
	differ := [][2]string{
		{"kafka.zk.KafkaZkClient$CheckedEphemeral failed", "kafka.zk.KafkaZkAdmin$CheckedEphemeral failed"},
		{"nmp_DeviceRequestFastDeviceProbe: state in doubt", "nmp_DeviceStartLoop: state in doubt"},
		{"java.lang.OutOfMemoryError: Java heap space", "java.lang.StackOverflowError: Java heap space"},
		{"task failed dag_id=etl_daily task_id=load", "task failed dag_id=etl_daily task_id=extract"},
		{"npm warn deprecated inflight@1.0.6: not supported", "npm warn deprecated glob@7.2.3: not supported"},
		{"postgresql@14-main.service: failed", "redis@6379.service: failed"},
		{"payment failed", "payment succeeded"},
	}
	for _, p := range differ {
		if Pattern(p[0]) == Pattern(p[1]) {
			t.Errorf("want two patterns: %q and %q -> %q", p[0], p[1], Pattern(p[0]))
		}
	}
}

// Letters-only ids after an id prefix, in any case, and pod names.
func TestPatternPrefixedIDsAndPods(t *testing.T) {
	same := [][2]string{
		// one capital (not enough for markCaseIDs), all small, all capitals
		{"[req_pxvjxvmUpm] could not get JDBC connection", "[req_QEPeJPPoTB] could not get JDBC connection"},
		{"[req_pxvjxvmupm] could not get JDBC connection", "[req_lvuxzawnwl] could not get JDBC connection"},
		{"session sess_KQZVTWPMXB expired", "session sess_ydpmkasyma expired"},
		{"charge failed for cus_gltbdfnsfr", "charge failed for cus_xkqzvbwmrt"}, // no vowel at all
		{"trace-xjqvkzplmw dropped", "trace-bvcxzwqklp dropped"},
		{`{"msg":"retry","job":"job_wqpxkzvbnm"}`, `{"msg":"retry","job":"job_hgfdsqwrtz"}`},
		// a Deployment's pods: hash and suffix in any mix of letters and
		// digits, a hash that reads as hex, another ReplicaSet
		{"Container orders-java in pod orders-java-5b7c9d-ktfjq was OOMKilled (exit code 137)", "Container orders-java in pod orders-java-5b7c9d-rvfj5 was OOMKilled (exit code 137)"},
		{"Container orders-java in pod orders-java-5b7c9d-x2k4p was OOMKilled (exit code 137)", "Container orders-java in pod orders-java-5b7c9d-ktfjq was OOMKilled (exit code 137)"},
		{"Back-off restarting failed container web in pod web-7d9f8b6c4-x2x9z_default(0b8f1c2d-3e4f-5a6b-7c8d-9e0f1a2b3c4d)", "Back-off restarting failed container web in pod web-56cf7bd9qm-wq7zn_default(1a2b3c4d-5e6f-7081-92a3-b4c5d6e7f809)"},
		{"Warning BackOff pod/coredns-7db6d8ff4d-pkhwz restarting", "Warning BackOff pod/coredns-7db6d8ff4d-mx2nq restarting"},
		{`"hostname":"api-bcdfghjk-ktfjq","msg":"slow query"`, `"hostname":"api-5d8f9c7b6-2bq4z","msg":"slow query"`},
		// a StatefulSet's pods
		{"pod kafka-0 restarted", "pod kafka-12 restarted"},
	}
	for _, p := range same {
		if a, b := Pattern(p[0]), Pattern(p[1]); a != b {
			t.Errorf("want one pattern:\n  %q -> %q\n  %q -> %q", p[0], a, p[1], b)
		}
	}
	differ := [][2]string{
		// words after an id prefix
		{"upstream request-timeout after 30s", "upstream request-received after 30s"},
		{"session_expired for the cart", "session_started for the cart"},
		{"job-scheduler stopped", "job-controller stopped"},
		{"trace-enabled in config", "trace-disabled in config"},
		{"span_context missing", "span_exporter missing"},
		{"msg_handler failed", "msg_dispatcher failed"},
		{"tx_packets dropped", "tx_carrier dropped"},
		{"order_created received", "order_cancelled received"}, // not an id prefix
		// an id prefix inside a longer name
		{"max_req_pxvjxvmupm reached", "max_req_lvuxzawnwl reached"},
		// hyphenated words and numbers that are no pod names
		{"kube-scheduler-leader elected", "kube-controller-leader elected"},
		{"check-in-progress", "check-in-complete"},
		{"listening on port-8443-https", "listening on port-8443-smtps"},
		{"retry-3-10s scheduled", "retry-3-10m scheduled"},
		{"nginx-ingress-backend-bcdfg up", "nginx-ingress-frontend-bcdfg up"},
	}
	for _, p := range differ {
		if Pattern(p[0]) == Pattern(p[1]) {
			t.Errorf("want two patterns: %q and %q -> %q", p[0], p[1], Pattern(p[0]))
		}
	}
	// A pattern saved before (letter case gone) renormalizes to today's,
	// so learned rules keep matching and migrate.
	for old, msg := range map[string]string{
		"[req_pxvjxvmupm] could not get jdbc connection":                          "[req_lvuXzawnWL] could not get JDBC connection",
		"container orders-java in pod orders-java-<id>-ktfjq was oomkilled":       "Container orders-java in pod orders-java-5b7c9d-x2k4p was OOMKilled",
		"container orders-java in pod orders-java-<id>-rvfj<n> was oomkilled":     "Container orders-java in pod orders-java-5b7c9d-x2k4p was OOMKilled",
		"back-off restarting failed container web in pod web-<hex>-<id>_default(": "Back-off restarting failed container web in pod web-56cf7bd9qm-wq7zn_default(",
	} {
		if got, want := Renormalize(old), Pattern(msg); got != want {
			t.Errorf("Renormalize(%q) = %q, want %q", old, got, want)
		}
	}
}

func TestPatternIdempotentAndRenormalizes(t *testing.T) {
	for _, m := range []string{
		"[req_e3LK2IIpMb] could not get JDBC connection: Connection to db:5432 refused",
		"Accepted publickey for deploy from 10.0.0.27 port 44257 ssh2: ED25519 SHA256:RNprkcmel7aeoQBLfbAxjNPvhyBTV97GR7tHQU2Cr2D",
		"user=<alice@example.com> session=<dGVzdHNlc3Npb24> pc=0x4ad370",
		strings.Repeat("order 42 failed for user=alice ", 30),
		"[req_pxvjxvmUpm] trace-xjqvkzplmw sess_KQZVTWPMXB failed",
		"Container orders-java in pod orders-java-5b7c9d-ktfjq was OOMKilled; pod/web-7d9f8b6c4-x2x9z and kafka-0",
	} {
		p := Pattern(m)
		if Pattern(p) != p || Normalize(p) != p || Renormalize(p) != p {
			t.Errorf("not idempotent: %q -> %q", m, p)
		}
		// a pattern saved before (digit ids) renormalizes to today's
		if len(m) <= 400 && Renormalize(LegacyPattern(m)) != p {
			t.Errorf("Renormalize(LegacyPattern(%q)) = %q, want %q", m, Renormalize(LegacyPattern(m)), p)
		}
	}
}

func TestLegacyLearnedPatternStillMatches(t *testing.T) {
	msg := "[req_e3LK2IIpMb] could not get JDBC connection"
	e := New([]config.Rule{{Name: "learned", Enabled: true, Match: config.MatchAll, Category: "database",
		Conditions: []config.Condition{{Field: "pattern", Op: config.OpEquals, Value: LegacyPattern(msg)}}}})
	ev := &model.Event{Message: "[req_9QpByXekYE] could not get JDBC connection"}
	e.Classify(ev)
	if ev.Category != "database" {
		t.Errorf("a legacy pattern should match other ids now: %+v", ev)
	}
}

func TestKeepStated(t *testing.T) {
	e := New([]config.Rule{{Name: "exc", Enabled: true, Match: config.MatchAll, Category: "application",
		Conditions: []config.Condition{{Field: "message", Op: config.OpContains, Value: "exception"}},
		Severity:   model.SeverityError, SeverityMode: config.SeverityAtLeast, KeepStated: true}})
	for _, c := range []struct {
		level, msg string
		want       model.Severity
	}{
		{"", "WARN SqlExceptionHelper: exception while flushing", model.SeverityWarning}, // stated in the text
		{"info", "exception messages may include data", model.SeverityInfo},              // a level field
		{"", "unexpected exception while flushing", model.SeverityError},                 // no level: the rule's
	} {
		ev := &model.Event{Level: c.level, Message: c.msg}
		e.Classify(ev)
		if ev.Severity != c.want || ev.Category != "application" {
			t.Errorf("%q: %s/%s, want %s/application", c.msg, ev.Severity, ev.Category, c.want)
		}
	}
}

// An account under attack is not its own problem; how a login went is.
func TestPatternAccountsAndOutcomes(t *testing.T) {
	same := [][2]string{
		{"Failed password for root from 203.0.113.9 port 52211 ssh2", "Failed password for admin from 203.0.113.9 port 52211 ssh2"},
		{"Accepted publickey for deploy from 10.0.0.5 port 50000 ssh2", "Accepted publickey for alice from 10.0.0.5 port 50000 ssh2"},
		{"auth event login=failed user=bob", "auth event login=failed user=carol"},
		{`{"event":"login","user":"bob","result":"denied"}`, `{"event":"login","user":"carol","result":"denied"}`},
	}
	for _, p := range same {
		if a, b := Pattern(p[0]), Pattern(p[1]); a != b {
			t.Errorf("want one pattern:\n  %q -> %q\n  %q -> %q", p[0], a, p[1], b)
		}
	}
	differ := [][2]string{
		{"auth event login=failed user=bob", "auth event login=success user=bob"},
		{`{"login":"failed","user":"bob"}`, `{"login":"ok","user":"bob"}`},
		{"mail email=bounced to=<x>", "mail email=delivered to=<x>"},
		{"account=locked after 5 attempts", "account=active after 5 attempts"},
	}
	for _, p := range differ {
		if Pattern(p[0]) == Pattern(p[1]) {
			t.Errorf("want two patterns: %q and %q -> %q", p[0], p[1], Pattern(p[0]))
		}
	}
	for _, msg := range []string{"Failed password for root from 203.0.113.9 port 52211 ssh2", "auth event login=failed user=bob"} {
		if p := Pattern(msg); Pattern(p) != p || Renormalize(p) != p {
			t.Errorf("not idempotent: %q", p)
		}
	}
}
