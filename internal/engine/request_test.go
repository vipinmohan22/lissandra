package engine

import (
	"strings"
	"testing"

	"github.com/vipinmohan22/lissandra/internal/model"
)

// The root cause of a multi-line record goes second in the state, where the
// engine reads it before the budget runs out.
func TestRootCauseOfRealTraces(t *testing.T) {
	for _, tc := range []struct{ name, msg, want string }{
		{"java", "Request processing failed\n\tat org.springframework.web.servlet.FrameworkServlet.processRequest(FrameworkServlet.java:1014)\nCaused by: org.hibernate.exception.JDBCConnectionException: Unable to acquire JDBC Connection\n\tat x.y(Z.java:1)\nCaused by: java.sql.SQLTransientConnectionException: HikariPool-1 - Connection is not available\n\tat com.zaxxer.hikari.pool.HikariPool.createTimeoutException(HikariPool.java:696)",
			"Caused by: java.sql.SQLTransientConnectionException: HikariPool-1 - Connection is not available"},
		{"python-psycopg", "Traceback (most recent call last):\n  File \"/app/svc.py\", line 42, in save\n    cur.execute(q, args)\npsycopg2.errors.UniqueViolation: duplicate key value violates unique constraint \"users_email_key\"\nDETAIL:  Key (email)=(a@b.c) already exists.",
			"psycopg2.errors.UniqueViolation: duplicate key value violates unique constraint \"users_email_key\""},
		{"python-chained", "Traceback (most recent call last):\n  File \"a.py\", line 1, in f\nKeyError: 'id'\n\nDuring handling of the above exception, another exception occurred:\n\nTraceback (most recent call last):\n  File \"a.py\", line 3, in g\nhttpx.ReadTimeout: The read operation timed out",
			"httpx.ReadTimeout: The read operation timed out"},
		{"go-panic", "panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a1b2c]\n\ngoroutine 1 [running]:\nmain.handleError(...)\n\t/app/main.go:20\nmain.main()\n\t/app/main.go:10 +0x25",
			"[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a1b2c]"}, // no stack frame promoted
		{"node-cause", "Error: failed to load user 42\n    at loadUser (/app/user.js:10:11)\n    at async handler (/app/server.js:22:5) {\n  [cause]: Error: connect ECONNREFUSED 127.0.0.1:5432\n      at TCPConnectWrap.afterConnect [as oncomplete] (node:net:1595:16)\n}",
			"[cause]: Error: connect ECONNREFUSED 127.0.0.1:5432"},
		{"dotnet-inner", "System.InvalidOperationException: An error occurred while saving the entity changes.\n ---> Microsoft.Data.SqlClient.SqlException (0x80131904): Cannot insert duplicate key row\n   at Microsoft.Data.SqlClient.SqlCommand.ExecuteReader()\n   --- End of inner exception stack trace ---",
			"---> Microsoft.Data.SqlClient.SqlException (0x80131904): Cannot insert duplicate key row"},
	} {
		ev := model.Event{Message: tc.msg}
		lines := strings.Split(buildState(&ev), "\n")
		if len(lines) < 2 || lines[1] != tc.want {
			t.Errorf("%s: state %q, want %q second", tc.name, lines, tc.want)
		}
	}
}

func TestIDFieldHeuristic(t *testing.T) {
	for k, skip := range map[string]bool{
		"request_id": true, "traceId": true, "x-request-id": true, "user": true, "ID": true,
		"PAID": false, "VALID": false, "valid": false, "EventID": false, "winlog.event_id": false,
		"rule.id": false, "vulnerability.id": false, "threat.technique.id": false, "status": false,
	} {
		if isSkipped(k) != skip {
			t.Errorf("isSkipped(%q) = %v", k, !skip)
		}
	}
}
