package main

// Tests for the email action (against a small fake SMTP server) and for syslog
// over TCP.

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturedMail struct {
	from string
	to   []string
	data string // headers and body as sent, CRLF line ends
}

// smtpServer is just enough SMTP to receive a message.
type smtpServer struct {
	addr          string
	mu            sync.Mutex
	conns         int
	mails         []capturedMail
	auths         []string // decoded AUTH PLAIN payloads, NUL shown as |
	offerAuth     bool
	failFirstData bool // answer the first DATA with 451 so the client must retry
	dataCalls     int
}

func newSMTPServer(t *testing.T) *smtpServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen:", err)
	}
	s := &smtpServer{addr: ln.Addr().String()}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			go s.handle(c)
		}
	}()
	return s
}

func (s *smtpServer) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(format string, a ...any) { fmt.Fprintf(c, format+"\r\n", a...) }
	say("220 fake ESMTP")
	var cur capturedMail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			s.mu.Lock()
			auth := s.offerAuth
			s.mu.Unlock()
			if auth {
				say("250-fake")
				say("250 AUTH PLAIN")
			} else {
				say("250 fake")
			}
		case strings.HasPrefix(up, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.Fields(line)[2])
			s.mu.Lock()
			s.auths = append(s.auths, strings.ReplaceAll(string(raw), "\x00", "|"))
			s.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			cur = capturedMail{from: strings.Trim(line[len("MAIL FROM:"):], "<> ")}
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			cur.to = append(cur.to, strings.Trim(line[len("RCPT TO:"):], "<> "))
			say("250 ok")
		case up == "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.dataCalls++
			fail := s.failFirstData && s.dataCalls == 1
			if !fail {
				cur.data = b.String()
				s.mails = append(s.mails, cur)
			}
			s.mu.Unlock()
			if fail {
				say("451 try again later")
			} else {
				say("250 queued")
			}
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (s *smtpServer) mailCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mails)
}

func emailDefaults(s *smtpServer) string {
	return fmt.Sprintf("[DEFAULTS] {smtp_server = %q, smtp_from = \"verifien@example.com\", smtp_to = \"ops@example.com, Me <me@example.com>\", smtp_tls = none}\n", s.addr)
}

func TestEmailSendsTheReport(t *testing.T) {
	s := newSMTPServer(t)
	o := run(t, emailDefaults(s)+"[web]\nck environ(\"VERIFIEN_SURELY_UNSET\", {msg \"first problem\"}, set == yes)\nck environ(\"VERIFIEN_SURELY_UNSET_2\", set == yes)\naction email")
	require.Empty(t, o.results[0].Actions[0].Err, "email action failed")
	require.Equal(t, 1, s.mailCount(), "one per section, not one per failure")
	m := s.mails[0]
	assert.Equal(t, "verifien@example.com", m.from)
	assert.Equal(t, []string{"ops@example.com", "me@example.com"}, m.to)
	msg, err := mail.ReadMessage(strings.NewReader(m.data))
	require.NoError(t, err)
	assert.Equal(t, "verifien: 2 failed check(s) in [web]", msg.Header.Get("Subject"))
	assert.True(t, strings.HasPrefix(msg.Header.Get("Content-Type"), "text/plain"), msg.Header.Get("Content-Type"))
	for _, h := range []string{"From", "To", "Date", "Message-Id"} {
		assert.NotEmpty(t, msg.Header.Get(h), "header %s is missing", h)
	}
	for _, want := range []string{"Section: web", "Failed checks: 2", "first problem", "VERIFIEN_SURELY_UNSET_2"} {
		assert.Contains(t, m.data, want)
	}
	assert.NotContains(t, strings.ReplaceAll(m.data, "\r\n", ""), "\n", "the message must use CRLF line ends only")
}

func TestEmailOptionsOverrideRecipientsAndSubject(t *testing.T) {
	s := newSMTPServer(t)
	run(t, emailDefaults(s)+"[a]\n"+failing+"\naction email {to \"only@example.com\", subject \"custom subject\"}")
	require.Equal(t, 1, s.mailCount())
	msg, err := mail.ReadMessage(strings.NewReader(s.mails[0].data))
	require.NoError(t, err)
	assert.Equal(t, []string{"only@example.com"}, s.mails[0].to)
	assert.Equal(t, "custom subject", msg.Header.Get("Subject"))
}

func TestEmailSubjectCannotInjectHeaders(t *testing.T) {
	s := newSMTPServer(t)
	run(t, emailDefaults(s)+"[a]\n"+failing+"\naction email {subject \"hello\\nBcc: evil@example.com\"}")
	require.Equal(t, 1, s.mailCount())
	msg, err := mail.ReadMessage(strings.NewReader(s.mails[0].data))
	require.NoError(t, err)
	assert.Empty(t, msg.Header.Get("Bcc"), "a newline in the subject added a header")
	assert.Contains(t, msg.Header.Get("Subject"), "evil@example.com", "the text should stay, on one line")
}

func TestEmailRetriesAfterATemporaryFailure(t *testing.T) {
	s := newSMTPServer(t)
	s.failFirstData = true
	o := run(t, emailDefaults(s)+"[a]\n"+failing+"\naction email {retry 2, sleep 0.1}")
	assert.Empty(t, o.results[0].Actions[0].Err)
	assert.Equal(t, 1, s.mailCount())
	assert.Equal(t, 2, s.conns, "the first attempt failed with 451")
}

func TestEmailAuthUsesThePasswordFromTheEnvironment(t *testing.T) {
	s := newSMTPServer(t)
	s.offerAuth = true
	t.Setenv("VERIFIEN_SMTP_PASSWORD", "s3cret")
	o := run(t, strings.Replace(emailDefaults(s), "}", ", smtp_user = \"robot\"}", 1)+"[a]\n"+failing+"\naction email")
	assert.Empty(t, o.results[0].Actions[0].Err)
	assert.Equal(t, []string{"|robot|s3cret"}, s.auths)
	assert.NotContains(t, o.out, "s3cret", "the password must never be printed")
}

func TestEmailAuthWithoutAPasswordFails(t *testing.T) {
	s := newSMTPServer(t)
	s.offerAuth = true
	t.Setenv("VERIFIEN_SMTP_PASSWORD", "")
	o := run(t, strings.Replace(emailDefaults(s), "}", ", smtp_user = \"robot\"}", 1)+"[a]\n"+failing+"\naction email")
	assert.Contains(t, o.results[0].Actions[0].Err, "VERIFIEN_SMTP_PASSWORD")
	assert.Equal(t, 0, s.mailCount())
}

func TestEmailRequiresSTARTTLSUnlessToldOtherwise(t *testing.T) {
	s := newSMTPServer(t)                                                // does not offer STARTTLS
	cfg := strings.Replace(emailDefaults(s), ", smtp_tls = none", "", 1) // the default is starttls
	o := run(t, cfg+"[a]\n"+failing+"\naction email")
	assert.Contains(t, o.results[0].Actions[0].Err, "does not offer STARTTLS")
	assert.Equal(t, 0, s.mailCount(), "a mail must not go out unencrypted by default")
}

func TestEmailConfigErrors(t *testing.T) {
	base := "smtp_server = \"127.0.0.1:2525\", smtp_from = \"a@example.com\", smtp_to = \"b@example.com\""
	use := "[a]\n" + failing + "\naction email"
	cases := []struct{ name, cfg, want string }{
		{"no server", "[DEFAULTS] {smtp_from = \"a@example.com\", smtp_to = \"b@example.com\"}\n" + use, "no server"},
		{"server without a port", "[DEFAULTS] {" + strings.Replace(base, "127.0.0.1:2525", "mail.example.com", 1) + "}\n" + use, "host:587"},
		{"no sender", "[DEFAULTS] {smtp_server = \"h:25\", smtp_to = \"b@example.com\"}\n" + use, "smtp_from"},
		{"bad sender", "[DEFAULTS] {smtp_server = \"h:25\", smtp_from = \"not an address\", smtp_to = \"b@example.com\"}\n" + use, "smtp_from"},
		{"no recipient", "[DEFAULTS] {smtp_server = \"h:25\", smtp_from = \"a@example.com\"}\n" + use, "no recipient"},
		{"bad recipient", "[DEFAULTS] {" + base + "}\n" + use[:len(use)-len("email")] + "email {to \"nope\"}", "recipients"},
		{"bad tls mode", "[DEFAULTS] {" + base + ", smtp_tls = maybe}\n" + use, "must be one of"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := load(c.cfg)
			assert.ErrorContains(t, err, c.want)
		})
	}
	// and a complete configuration is accepted
	mustLoad(t, "[DEFAULTS] {"+base+"}\n[a]\n"+failing+"\naction email {subject \"x\"}")
}

func TestSyslogOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen:", err)
	}
	defer ln.Close()
	got := make(chan string, 4)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			got <- sc.Text()
		}
	}()
	run(t, fmt.Sprintf("[DEFAULTS] {syslog_address = \"tcp://%s\", syslog_tag = \"vtcp\", syslog_facility = daemon}\n[web]\n%s\nck environ(\"VERIFIEN_SURELY_UNSET_2\", set == yes)\naction syslog", ln.Addr(), failing))
	for i := 0; i < 2; i++ { // one message per failed ck
		select {
		case line := <-got:
			// <27> is daemon (3*8) + err (3)
			for _, want := range []string{"<27>", "vtcp[", `section="web"`} {
				assert.Contains(t, line, want, "syslog line %d", i)
			}
		case <-time.After(3 * time.Second):
			require.Failf(t, "timeout", "syslog line %d never arrived", i)
		}
	}
}
