// Package metrics holds the functions that look at metrics other programs
// publish. Each file registers its function from init().
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"verifien/spec"
)

// maxBody is the most of a metrics page that is read. A node_exporter page is
// about 0.5 MB; this is generous, and stops a runaway server filling memory.
const maxBody = 32 << 20

func init() {
	spec.Register("prometheus", spec.FuncSpec{
		Run:     prometheus,
		Async:   true,
		Options: spec.StdOptions(),
		// No fixed items. Every item name in a test is a label (see spec.LabelField).
		Tests:  map[string]spec.Field{},
		Labels: true,
	})
}

// prometheus("URL?metric_name", label OP value, ...) fetches a page in the
// Prometheus text format and passes when at least one series of that metric has
// all the labels the tests describe. A label the series does not have counts as
// "" (as in PromQL), so `mode == ""` and `mode != "idle"` work on a series with
// no mode label.
func prometheus(ctx context.Context, req spec.Request) (spec.Result, error) {
	page, metric, err := splitTarget(req.Target)
	if err != nil {
		return nil, err
	}
	series, err := spec.Attempt(ctx, req, func() ([]map[string]string, error) {
		return fetch(ctx, page, metric, req)
	})
	if err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, fmt.Errorf("prometheus: no metric named %s at %s", metric, page.Redacted())
	}
	for _, labels := range series {
		if matches(labels, req.Tests) {
			return spec.Result{}, nil
		}
	}
	return nil, fmt.Errorf("prometheus: none of the %d series of %s has %s", len(series), metric, describe(req.Tests))
}

// splitTarget separates the page URL from the metric name. The metric name is
// what follows the "?" (http://host:9100/metrics?node_cpu_info), or the value of
// a "name" parameter (http://host:9100/metrics?name=node_cpu_info), which is
// removed from the URL that is fetched. Any other parameters stay. A trailing
// slash on the path is removed too: /metrics/ becomes /metrics.
func splitTarget(target string) (*url.URL, string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, "", fmt.Errorf("prometheus: bad URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, "", fmt.Errorf("prometheus: the target must be an http:// or https:// URL followed by ?metric_name")
	}
	// Exporters answer /metrics but not /metrics/, so a trailing slash is dropped.
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	var metric string
	if !strings.Contains(u.RawQuery, "=") {
		metric, err = url.QueryUnescape(u.RawQuery)
		if err != nil {
			return nil, "", fmt.Errorf("prometheus: bad metric name: %v", err)
		}
		u.RawQuery = ""
	} else {
		q := u.Query()
		metric = q.Get("name")
		q.Del("name")
		u.RawQuery = q.Encode()
	}
	if metric == "" {
		return nil, "", fmt.Errorf("prometheus: the target needs a metric name: URL?metric_name or URL?name=metric_name")
	}
	return u, metric, nil
}

// fetch reads the page and returns the label sets of every series of the metric.
func fetch(ctx context.Context, page *url.URL, metric string, req spec.Request) ([]map[string]string, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, page.String(), nil)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: %s answered %s", page.Redacted(), resp.Status)
	}
	return parse(io.LimitReader(resp.Body, maxBody+1), metric)
}

// parse reads the text exposition format and returns the label sets of the
// samples named metric. Comment lines (# HELP, # TYPE, # EOF) are skipped; a
// line of that metric that cannot be read is an error.
func parse(r io.Reader, metric string) ([]map[string]string, error) {
	var out []map[string]string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	read := 0
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		read += len(line) + 1
		if read > maxBody {
			return nil, fmt.Errorf("prometheus: the page is larger than %d MiB", maxBody>>20)
		}
		if line == "" || line[0] == '#' {
			continue
		}
		// cheap test first: most lines belong to other metrics
		if !strings.HasPrefix(line, "{") {
			rest, ok := strings.CutPrefix(line, metric)
			if !ok || rest == "" || (rest[0] != '{' && rest[0] != ' ' && rest[0] != '\t') {
				continue
			}
		}
		name, labels, err := parseSample(line)
		if err != nil {
			return nil, fmt.Errorf("prometheus: line %d: %v", n, err)
		}
		if name == metric {
			out = append(out, labels)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("prometheus: %v", err)
	}
	return out, nil
}

// parseSample reads `name{label="value",...} number [timestamp]`. It also reads
// the quoted form that allows any UTF-8 name: `{"metric.name","label"="value"}`.
func parseSample(line string) (string, map[string]string, error) {
	p := &lineParser{s: line}
	name := p.bareName()
	labels := map[string]string{}
	if p.eat('{') {
		for {
			p.space()
			if p.eat('}') {
				break
			}
			key, err := p.labelName()
			if err != nil {
				return "", nil, err
			}
			p.space()
			if p.eat('=') {
				p.space()
				val, err := p.quoted()
				if err != nil {
					return "", nil, err
				}
				labels[key] = val
			} else if name == "" {
				name = key // {"quoted.name", ...}
			} else {
				return "", nil, fmt.Errorf("label %q has no value", key)
			}
			p.space()
			if p.eat(',') {
				continue
			}
			if p.eat('}') {
				break
			}
			return "", nil, fmt.Errorf("expected , or } after label %q", key)
		}
	}
	if name == "" {
		return "", nil, fmt.Errorf("no metric name")
	}
	fields := strings.Fields(p.s[p.i:])
	if len(fields) < 1 || len(fields) > 2 {
		return "", nil, fmt.Errorf("expected a value (and an optional timestamp) after the labels")
	}
	if _, err := strconv.ParseFloat(fields[0], 64); err != nil {
		return "", nil, fmt.Errorf("bad sample value %q", fields[0])
	}
	return name, labels, nil
}

type lineParser struct {
	s string
	i int
}

func (p *lineParser) eat(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *lineParser) space() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

// bareName reads [a-zA-Z0-9_:]* (possibly empty).
func (p *lineParser) bareName() string {
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '_' || c == ':' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			p.i++
			continue
		}
		break
	}
	return p.s[start:p.i]
}

// labelName reads a label name, bare or quoted.
func (p *lineParser) labelName() (string, error) {
	if p.i < len(p.s) && p.s[p.i] == '"' {
		return p.quoted()
	}
	if n := p.bareName(); n != "" {
		return n, nil
	}
	return "", fmt.Errorf("expected a label name at %q", p.s[p.i:])
}

// quoted reads "text" with the escapes \\ \" \n.
func (p *lineParser) quoted() (string, error) {
	if !p.eat('"') {
		return "", fmt.Errorf("expected a quoted value at %q", p.s[p.i:])
	}
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.i >= len(p.s) {
				return "", fmt.Errorf("unterminated escape")
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case '\\', '"':
				b.WriteByte(e)
			default:
				return "", fmt.Errorf(`unknown escape \%c`, e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("unterminated quoted value")
}

// matches reports whether one series (its labels) satisfies every test. A label
// the series does not have is "".
func matches(labels map[string]string, tests []spec.TestRef) bool {
	res := spec.Result{}
	for _, t := range tests {
		res[t.Item] = labels[t.Item]
	}
	for _, t := range tests {
		if ok, _ := spec.Evaluate("prometheus", res, t.Item, t.Op, t.Want); !ok {
			return false
		}
	}
	return true
}

// describe writes the tests the way they were written: package == 0, mode != "idle".
func describe(tests []spec.TestRef) string {
	if len(tests) == 0 {
		return "no tests"
	}
	parts := make([]string, len(tests))
	for i, t := range tests {
		parts[i] = fmt.Sprintf("%s %s %s", t.Item, t.Op, t.Want.Literal())
	}
	return strings.Join(parts, ", ")
}
