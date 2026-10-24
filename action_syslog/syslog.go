// Package actionsyslog is the syslog action: one error-level syslog message per
// failed ck.
//
// A remote server (syslog_address) is reached with a small RFC 3164 client in
// this file, so it works on every platform. The local syslog daemon is reached
// with log/syslog, in local.go.
package actionsyslog

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"verifien/spec"
)

// facilityNumber maps each facility name to its number (RFC 3164, table 2).
var facilityNumber = map[string]int{
	"kern": 0, "user": 1, "mail": 2, "daemon": 3, "auth": 4, "syslog": 5,
	"lpr": 6, "news": 7, "uucp": 8, "cron": 9, "authpriv": 10, "ftp": 11,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19,
	"local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

// facilities lists the names in a fixed order for the error message.
var facilities = []string{
	"kern", "user", "mail", "daemon", "auth", "syslog", "lpr", "news", "uucp",
	"cron", "authpriv", "ftp",
	"local0", "local1", "local2", "local3", "local4", "local5", "local6", "local7",
}

const severityErr = 3

// The three values can be set in [DEFAULTS] and overridden for one use:
//
//	[DEFAULTS] {syslog_tag "verifien", syslog_facility local3, syslog_address "udp://loghost:514"}
//	action syslog {syslog_tag "web", syslog_facility daemon}
var fields = map[string]spec.Field{
	"syslog_tag":      {Kind: spec.KString},                 // default "verifien"
	"syslog_facility": {Kind: spec.KEnum, Enum: facilities}, // default user
	"syslog_address":  {Kind: spec.KString},                 // udp://host:514 or tcp://host:514; default: local syslog
}

func init() {
	spec.RegisterAction("syslog", spec.ActionSpec{
		Run:      run,
		Settings: fields, // [DEFAULTS]
		Options:  fields, // action syslog {...}
		Check: func(req spec.ActionRequest) error {
			if a := get(req, "syslog_address"); a != "" {
				if _, _, err := parseAddress(a); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

// get returns the action's own option, else the [DEFAULTS] setting, else "".
func get(req spec.ActionRequest, key string) string {
	if v := req.Opt(key); v != "" {
		return v
	}
	return req.Setting(key)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func run(ctx context.Context, req spec.ActionRequest) error {
	tag := orDefault(get(req, "syslog_tag"), "verifien")
	facility := facilityNumber[orDefault(get(req, "syslog_facility"), "user")]
	address := get(req, "syslog_address")

	lines := make([]string, 0, len(req.Report.Failures))
	for _, f := range req.Report.Failures {
		lines = append(lines, fmt.Sprintf("section=%q ck=%q message=%q", req.Report.Section, f.Ck, f.Message))
	}
	return req.Attempt(ctx, func(actx context.Context) error {
		if address == "" {
			return sendLocal(facility, tag, lines)
		}
		network, host, err := parseAddress(address)
		if err != nil {
			return err
		}
		return sendRemote(actx, network, host, facility*8+severityErr, tag, lines)
	})
}

// parseAddress splits "udp://host:514" into ("udp", "host:514").
func parseAddress(a string) (network, host string, err error) {
	for _, n := range []string{"udp", "tcp"} {
		if rest, ok := strings.CutPrefix(a, n+"://"); ok && rest != "" {
			return n, rest, nil
		}
	}
	return "", "", fmt.Errorf("syslog_address %q must look like udp://host:514 or tcp://host:514", a)
}

// sendRemote writes one RFC 3164 message per line:
//
//	<PRI>Mmm dd hh:mm:ss HOST TAG[pid]: text
//
// Over UDP each message is a datagram (cut to the 1024 bytes the RFC allows);
// over TCP each message ends with a newline.
func sendRemote(ctx context.Context, network, host string, priority int, tag string, lines []string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, host)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() }) // end a blocked write at the timeout
	defer stop()

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	for _, l := range lines {
		l = strings.NewReplacer("\r", " ", "\n", " ").Replace(l)
		msg := fmt.Sprintf("<%d>%s %s %s[%d]: %s", priority, time.Now().Format(time.Stamp), hostname, tag, os.Getpid(), l)
		if network == "udp" && len(msg) > 1024 {
			msg = msg[:1024]
		}
		if network == "tcp" {
			msg += "\n"
		}
		if _, err := conn.Write([]byte(msg)); err != nil {
			return err
		}
	}
	return nil
}
