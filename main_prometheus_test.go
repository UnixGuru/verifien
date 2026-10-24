package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const metricsPage = `# TYPE node_cpu_info gauge
node_cpu_info{core="0",package="0"} 1
node_cpu_info{core="1",package="0"} 1
node_cpu_info{core="0",package="1"} 1
node_load1 0.5
`

func metricsServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, metricsPage)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPrometheusInAConfig(t *testing.T) {
	u := metricsServer(t)
	stmts := func(cfg string) []StmtResult {
		o := run(t, cfg)
		return o.results[0].Stmts
	}
	// the form from the request, trailing slash and name= included
	s := stmts(fmt.Sprintf("[a]\nck prometheus(%q, package == 0)", u+"/metrics/?name=node_cpu_info"))
	assert.True(t, s[0].Pass, s[0].Detail)
	assert.Contains(t, s[0].Text, `prometheus("`+u+`/metrics/?name=node_cpu_info", package == 0)`)

	o := run(t, fmt.Sprintf("[a]\nck prometheus(%q, package == 1, core == 0)\nck prometheus(%q, package == 1, core == 1)\nif ! prometheus(%q, package == 5)\nck prometheus(%q, {msg \"custom\"}, package != 0)",
		u+"/metrics?node_cpu_info", u+"/metrics?node_cpu_info", u+"/metrics?node_cpu_info", u+"/metrics?node_cpu_info"))
	for _, st := range o.results[0].Stmts {
		switch {
		case strings.HasSuffix(st.Text, "core == 1)"):
			assert.False(t, st.Pass)
			assert.Contains(t, st.Detail, "none of the 3 series of node_cpu_info")
		default:
			assert.True(t, st.Pass, "%s: %s", st.Text, st.Detail)
		}
	}

	// msg replaces the failure text; a missing metric and a bad URL fail, not crash
	s = stmts(fmt.Sprintf("[a]\nck prometheus(%q, {msg \"cpu gone\"}, package == 9)\nck prometheus(%q)\nck prometheus(\"http://127.0.0.1:1/metrics?x\", {timeout 1})",
		u+"/metrics?node_cpu_info", u+"/metrics?missing"))
	assert.Equal(t, "cpu gone", s[0].Detail)
	assert.Contains(t, s[1].Detail, "no metric named missing")
	assert.False(t, s[2].Pass)

	// no tests at all: the metric just has to exist; a variable gates nothing
	s = stmts(fmt.Sprintf("[a]\np = prometheus(%q)\nck prometheus(%q)", u+"/metrics?node_load1", u+"/metrics?node_load1"))
	assert.True(t, s[0].Pass && s[1].Pass)
}

func TestPrometheusLabelFromAVariable(t *testing.T) {
	u := metricsServer(t)
	t.Setenv("VERIFIEN_PKG", "1")
	s := run(t, fmt.Sprintf("[a]\ne = environ(\"VERIFIEN_PKG\")\nck prometheus(%q, package == e.value, core == 0)\nck prometheus(%q, package != e.value, core == 1)",
		u+"/metrics?node_cpu_info", u+"/metrics?node_cpu_info")).results[0].Stmts
	assert.True(t, s[1].Pass, s[1].Detail)
	assert.True(t, s[2].Pass, s[2].Detail)
	// any item can be a label's value, by its text: nlink is 1 or more, never "0"
	mustLoad(t, fmt.Sprintf("[a]\nx = lstat(\"/\")\nck prometheus(%q, package == x.nlink)", u+"/metrics?node_cpu_info"))
}

func TestPrometheusConfigErrors(t *testing.T) {
	for name, c := range map[string]struct{ cfg, want string }{
		"label with <":    {`[a]` + "\n" + `ck prometheus("http://h/metrics?m", package < 1)`, "operator < not allowed"},
		"regex literal":   {`[a]` + "\n" + `ck prometheus("http://h/metrics?m", package == /x/)`, "expected a string"},
		"bad regex":       {`[a]` + "\n" + `ck prometheus("http://h/metrics?m", package =~ "(")`, "invalid regex"},
		"test in assign":  {`[a]` + "\n" + `p = prometheus("http://h/metrics?m", package == 0)`, "would gate nothing"},
		"no items on var": {`[a]` + "\n" + `p = prometheus("http://h/metrics?m")` + "\n" + `ck p.package == 0`, "has no test"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := load(c.cfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, c.want)
		})
	}
	mustLoad(t, `[a]`+"\n"+`ck prometheus("http://h/metrics?m", package == 0, model =~ /EPYC/, mode != "idle", x == 'y')`)
}

// A slow exporter: the first request outlasts the timeout, so the call only
// passes if the call's {timeout, retry, sleep} options (or [DEFAULTS]) are used.
func TestPrometheusHonoursOptionsAndDefaults(t *testing.T) {
	newServer := func() (string, *atomic.Int32) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) == 1 {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			}
			fmt.Fprint(w, metricsPage)
		}))
		t.Cleanup(srv.Close)
		return srv.URL + "/metrics?node_cpu_info", &hits
	}

	u, hits := newServer()
	o := run(t, fmt.Sprintf("[a]\nck prometheus(%q, {timeout 1, retry 1, sleep 0.1}, package == 0)", u))
	assert.True(t, o.results[0].Stmts[0].Pass, o.out)
	assert.EqualValues(t, 2, hits.Load())

	// without retry the slow first attempt is the end of it
	u, hits = newServer()
	o = run(t, fmt.Sprintf("[a]\nck prometheus(%q, {timeout 1}, package == 0)", u))
	assert.False(t, o.results[0].Stmts[0].Pass)
	assert.EqualValues(t, 1, hits.Load())

	// [DEFAULTS] supply them when the call has none; the call's own option wins
	u, _ = newServer()
	o = run(t, fmt.Sprintf("[DEFAULTS] {timeout 1, retry 1}\n[a]\nck prometheus(%q, package == 0)", u))
	assert.True(t, o.results[0].Stmts[0].Pass, o.out)
	u, _ = newServer()
	o = run(t, fmt.Sprintf("[DEFAULTS] {timeout 1, retry 1}\n[a]\nck prometheus(%q, {retry 0}, package == 0)", u))
	assert.False(t, o.results[0].Stmts[0].Pass)
}
