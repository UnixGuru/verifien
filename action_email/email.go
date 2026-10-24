// Package actionemail is the email action: one plain-text message per section
// that lists the failed cks, sent over SMTP.
//
// Settings in [DEFAULTS] (the action's own {to "..."} and {subject "..."}
// options override the recipient list and the subject):
//
//	smtp_server  "host:587"                         required
//	smtp_from    "verifien@example.com"             required
//	smtp_to      "ops@example.com, me@example.com"  required unless the action gives {to ...}
//	smtp_user    "login"                            optional: log in with AUTH PLAIN
//	smtp_tls     starttls (default), tls, none
//
// The password is never read from the config file. Put it in the environment
// variable VERIFIEN_SMTP_PASSWORD.
package actionemail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"time"

	"verifien/spec"
)

const passwordEnv = "VERIFIEN_SMTP_PASSWORD"

func init() {
	spec.RegisterAction("email", spec.ActionSpec{
		Run: run,
		Options: map[string]spec.Field{
			"to":      {Kind: spec.KString}, // comma-separated; else smtp_to
			"subject": {Kind: spec.KString}, // else "verifien: N failed check(s) in [section]"
		},
		Settings: map[string]spec.Field{
			"smtp_server": {Kind: spec.KString},
			"smtp_from":   {Kind: spec.KString},
			"smtp_to":     {Kind: spec.KString},
			"smtp_user":   {Kind: spec.KString},
			"smtp_tls":    {Kind: spec.KEnum, Enum: []string{"starttls", "tls", "none"}},
		},
		Check: func(req spec.ActionRequest) error {
			_, err := configure(req)
			return err
		},
	})
}

type config struct {
	server  string // host:port
	host    string
	from    string
	to      []string
	user    string
	pass    string
	tlsMode string // starttls, tls, none
}

func configure(req spec.ActionRequest) (config, error) {
	c := config{
		server:  req.Setting("smtp_server"),
		user:    req.Setting("smtp_user"),
		pass:    os.Getenv(passwordEnv),
		tlsMode: req.Setting("smtp_tls"),
	}
	if c.tlsMode == "" {
		c.tlsMode = "starttls"
	}
	if c.server == "" {
		return c, errors.New(`no server: set smtp_server = "host:587" in [DEFAULTS]`)
	}
	host, _, err := net.SplitHostPort(c.server)
	if err != nil {
		return c, fmt.Errorf(`smtp_server %q must look like host:587`, c.server)
	}
	c.host = host

	from, err := mail.ParseAddress(req.Setting("smtp_from"))
	if err != nil {
		return c, fmt.Errorf(`smtp_from must be an address such as "verifien@example.com" (%v)`, err)
	}
	c.from = from.Address

	recipients := req.Opt("to")
	if recipients == "" {
		recipients = req.Setting("smtp_to")
	}
	if strings.TrimSpace(recipients) == "" {
		return c, errors.New(`no recipient: give {to "ops@example.com"} or set smtp_to in [DEFAULTS]`)
	}
	list, err := mail.ParseAddressList(recipients)
	if err != nil {
		return c, fmt.Errorf("recipients %q: %v", recipients, err)
	}
	for _, a := range list {
		c.to = append(c.to, a.Address)
	}
	return c, nil
}

// oneLine removes line breaks so text cannot add headers.
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// message builds the whole mail, headers and body, with CRLF line ends.
func message(cfg config, rep spec.Report, subject string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "Section: %s\nTime: %s\nFailed checks: %d\n\n", rep.Section, rep.Time, len(rep.Failures))
	for i, f := range rep.Failures {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, f.Ck, strings.ReplaceAll(f.Message, "\n", "\n   "))
	}
	body := strings.ReplaceAll(strings.ReplaceAll(b.String(), "\r\n", "\n"), "\n", "\r\n")

	var h strings.Builder
	header := func(k, v string) { fmt.Fprintf(&h, "%s: %s\r\n", k, v) }
	header("From", cfg.from)
	header("To", strings.Join(cfg.to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", oneLine(subject)))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", fmt.Sprintf("<%d.%d@verifien>", time.Now().UnixNano(), os.Getpid()))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=UTF-8")
	header("Content-Transfer-Encoding", "8bit")
	return []byte(h.String() + "\r\n" + body)
}

func run(ctx context.Context, req spec.ActionRequest) error {
	cfg, err := configure(req)
	if err != nil {
		return err
	}
	subject := req.Opt("subject")
	if subject == "" {
		subject = fmt.Sprintf("verifien: %d failed check(s) in [%s]", len(req.Report.Failures), req.Report.Section)
	}
	msg := message(cfg, req.Report, subject)
	return req.Attempt(ctx, func(actx context.Context) error { return send(actx, cfg, msg) })
}

// send delivers one message. The whole conversation ends at actx's deadline.
func send(ctx context.Context, cfg config, msg []byte) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", cfg.server)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	tlsConfig := &tls.Config{ServerName: cfg.host}
	var wire net.Conn = conn
	if cfg.tlsMode == "tls" {
		wire = tls.Client(conn, tlsConfig)
	}
	c, err := smtp.NewClient(wire, cfg.host)
	if err != nil {
		return err
	}
	defer c.Close()

	if cfg.tlsMode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New(`the server does not offer STARTTLS; use smtp_tls = none only on a network you trust`)
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if cfg.user != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("the server does not offer AUTH")
		}
		if cfg.pass == "" {
			return fmt.Errorf("smtp_user is set but %s is empty", passwordEnv)
		}
		// PlainAuth itself refuses to send the password over an unencrypted
		// connection to anything but localhost.
		if err := c.Auth(smtp.PlainAuth("", cfg.user, cfg.pass, cfg.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(cfg.from); err != nil {
		return err
	}
	for _, to := range cfg.to {
		if err := c.Rcpt(to); err != nil {
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
