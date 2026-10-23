package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"html/template"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/vipinmohan22/lissandra/internal/model"
)

func init() {
	Register(&Plugin{
		Type:        "email",
		Label:       "Email (SMTP)",
		Description: "Send an HTML + plain-text email through any SMTP server (SES, SendGrid, Postfix, Office 365, Gmail, …).",
		Audience:    "The addresses in To.",
		Fields: []Field{
			{Key: "smtp_host", Label: "SMTP host", Type: "text", Required: true, Placeholder: "smtp.example.com"},
			{Key: "smtp_port", Label: "SMTP port", Type: "number", Default: "587"},
			{Key: "security", Label: "Security", Type: "select", Options: []string{"starttls", "tls", "none"}, Default: "starttls", Help: "starttls = port 587, tls = implicit TLS on port 465, none = plaintext (local relays only)."},
			{Key: "username", Label: "Username", Type: "text"},
			{Key: "password", Label: "Password", Type: "password", Secret: true, Placeholder: "${env:SMTP_PASSWORD}"},
			{Key: "from", Label: "From", Type: "text", Required: true, Placeholder: "Lissandra <lissandra@example.com>"},
			{Key: "to", Label: "To", Type: "text", Required: true, Placeholder: "oncall@example.com, team@example.com", Help: "Comma-separated recipients.", Recipient: true},
			{Key: "subject_prefix", Label: "Subject prefix", Type: "text", Default: "[Lissandra]"},
			{Key: "insecure_skip_verify", Label: "Skip TLS certificate verification", Type: "checkbox", Help: "Only for internal relays with self-signed certificates."},
		},
		New: newEmail,
	})
}

type email struct {
	host, security, username, password, subjectPrefix string
	port                                              int
	from                                              *mail.Address
	to                                                []*mail.Address
	skipVerify                                        bool
}

func newEmail(s map[string]string) (Sender, error) {
	port, err := strconv.Atoi(s["smtp_port"])
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid SMTP port %q", s["smtp_port"])
	}
	from, err := mail.ParseAddress(s["from"])
	if err != nil {
		return nil, fmt.Errorf("invalid From address: %w", err)
	}
	to, err := mail.ParseAddressList(s["to"])
	if err != nil || len(to) == 0 {
		return nil, fmt.Errorf("invalid To addresses: %v", err)
	}
	return &email{
		host: s["smtp_host"], port: port, security: s["security"],
		username: s["username"], password: s["password"],
		from: from, to: to, subjectPrefix: s["subject_prefix"],
		skipVerify: s["insecure_skip_verify"] == "true",
	}, nil
}

func (e *email) Send(ctx context.Context, m *Message) error {
	msg, err := e.compose(m)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	addr := net.JoinHostPort(e.host, strconv.Itoa(e.port))
	tlsCfg := &tls.Config{ServerName: e.host, InsecureSkipVerify: e.skipVerify, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{Deadline: deadline}

	var conn net.Conn
	if e.security == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, e.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if e.security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("server does not support STARTTLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if e.username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.username, e.password, e.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(e.from.Address); err != nil {
		return err
	}
	for _, rcpt := range e.to {
		if err := c.Rcpt(rcpt.Address); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

var emailHTML = template.Must(template.New("email").Parse(`<!doctype html>
<html><body style="margin:0;padding:24px;background:#f4f5f7;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#1d2330">
<table role="presentation" width="100%" style="max-width:680px;margin:0 auto;background:#ffffff;border-radius:8px;border-top:6px solid {{.Color}}">
<tr><td style="padding:20px 24px">
<div style="font-size:18px;font-weight:600;margin-bottom:12px">{{.Title}}</div>
<pre style="white-space:pre-wrap;word-break:break-word;background:#f6f8fa;border-radius:6px;padding:12px;font-size:13px;margin:0 0 16px">{{.Excerpt}}</pre>
<table role="presentation" style="font-size:13px;border-collapse:collapse">
{{range .Facts}}<tr><td style="padding:3px 16px 3px 0;color:#5b6475;white-space:nowrap;vertical-align:top">{{.Name}}</td><td style="padding:3px 0">{{.Value}}</td></tr>{{end}}
</table>
{{if .Link}}<p style="margin:20px 0 0"><a href="{{.Link}}" style="background:#1d2330;color:#fff;text-decoration:none;padding:8px 14px;border-radius:6px;font-size:13px">Open in Lissandra</a></p>{{end}}
</td></tr></table>
<p style="text-align:center;font-size:11px;color:#8a8f98">Sent by Lissandra</p>
</body></html>`))

func (e *email) compose(m *Message) ([]byte, error) {
	var htmlBody bytes.Buffer
	err := emailHTML.Execute(&htmlBody, map[string]any{
		"Color": severityColor(m.Event.Severity), "Title": m.Title(), "Excerpt": m.Excerpt(8000),
		"Facts": m.Facts(), "Link": m.Link,
	})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	subject := strings.TrimSpace(e.subjectPrefix + " " + m.Title() + ": " + model.Truncate(strings.Join(strings.Fields(m.Event.Message), " "), 120))
	toList := make([]string, len(e.to))
	for i, a := range e.to {
		toList[i] = a.String()
	}
	hdr := []string{
		"From: " + e.from.String(),
		"To: " + strings.Join(toList, ", "),
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"Date: " + time.Now().Format(time.RFC1123Z),
		fmt.Sprintf("Message-ID: <%s@lissandra>", model.NewID()),
		"MIME-Version: 1.0",
		"Content-Type: multipart/alternative; boundary=" + mw.Boundary(),
		"X-Lissandra-Severity: " + string(m.Event.Severity),
		"X-Lissandra-Fingerprint: " + m.Event.Fingerprint,
	}
	head := strings.Join(hdr, "\r\n") + "\r\n\r\n"

	for _, part := range []struct{ ctype, body string }{
		{"text/plain; charset=utf-8", m.PlainText()},
		{"text/html; charset=utf-8", htmlBody.String()},
	} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.ctype},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write([]byte(part.body)); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return append([]byte(head), buf.Bytes()...), nil
}
