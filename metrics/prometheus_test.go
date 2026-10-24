package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"verifien/spec"
)

const page = `# HELP node_cpu_info CPU information
# TYPE node_cpu_info gauge
node_cpu_info{cachesize="512 KB",core="0",package="0",model_name="AMD \"EPYC\""} 1
node_cpu_info{core="1",package="0"} 1 1700000000000
node_cpu_info{core="0",package="1"} 1
node_cpu_infox{package="9"} 1
node_load1 0.42
node_network_up{device="eth0"} NaN
{"dotted.metric","some label"="a\\b"} 3
# EOF
`

func serve(t *testing.T) (*httptest.Server, *atomic.Value) {
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(r.URL.String())
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func tr(item, op, text string) spec.TestRef {
	return spec.TestRef{Item: item, Op: op, Want: spec.Value{String: &text}}
}

func call(t *testing.T, target string, tests ...spec.TestRef) error {
	t.Helper()
	fs, ok := spec.Lookup("prometheus")
	require.True(t, ok)
	_, err := fs.Run(context.Background(), spec.Request{Target: target, Timeout: 2 * time.Second, Tests: tests})
	return err
}

func TestSplitTarget(t *testing.T) {
	for in, want := range map[string][3]string{
		"http://h:1/metrics?node_cpu_info":        {"http://h:1/metrics", "node_cpu_info"},
		"http://h:1/metrics/?node_cpu_info":       {"http://h:1/metrics", "node_cpu_info"},
		"http://h:1/metrics/?name=node_cpu_info":  {"http://h:1/metrics", "node_cpu_info"},
		"http://h:1/metrics?name=a&collect[]=cpu": {"http://h:1/metrics?collect%5B%5D=cpu", "a"},
		"https://h/metrics?name=up":               {"https://h/metrics", "up"},
		"http://h:1/metrics?some%3Aname":          {"http://h:1/metrics", "some:name"},
	} {
		u, metric, err := splitTarget(in)
		require.NoError(t, err, in)
		assert.Equal(t, want[0], u.String(), in)
		assert.Equal(t, want[1], metric, in)
	}
	for in, msg := range map[string]string{
		"http://h:1/metrics":     "needs a metric name",
		"http://h:1/metrics?":    "needs a metric name",
		"http://h:1/metrics?x=1": "needs a metric name",
		"/metrics?up":            "http:// or https://",
		"ftp://h/metrics?up":     "http:// or https://",
		"node_cpu_info":          "http:// or https://",
		"http://h/metrics?%zz":   "bad metric name",
	} {
		_, _, err := splitTarget(in)
		assert.ErrorContains(t, err, msg, in)
	}
}

func TestParse(t *testing.T) {
	got, err := parse(strings.NewReader(page), "node_cpu_info")
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, `AMD "EPYC"`, got[0]["model_name"])
	assert.Equal(t, "512 KB", got[0]["cachesize"])
	assert.Equal(t, "1", got[2]["package"])
	got, err = parse(strings.NewReader(page), "node_load1")
	require.NoError(t, err)
	assert.Equal(t, []map[string]string{{}}, got)
	got, _ = parse(strings.NewReader(page), "node_network_up")
	assert.Len(t, got, 1) // NaN is a number
	got, err = parse(strings.NewReader(page), "dotted.metric")
	require.NoError(t, err)
	assert.Equal(t, `a\b`, got[0]["some label"])
	got, _ = parse(strings.NewReader(page), "missing")
	assert.Empty(t, got)
	for _, bad := range []string{`m{a="1} 1`, `m{a=1} 1`, `m{a="1"} x`, `m{a="1"}`, `m{a="\q"} 1`, `m{a} 1`, `m{a="1" b="2"} 1`, `m 1 2 3`} {
		_, err := parse(strings.NewReader(bad), "m")
		assert.Error(t, err, bad)
	}
}

func TestPrometheusLabels(t *testing.T) {
	srv, last := serve(t)
	base := srv.URL + "/metrics"
	require.NoError(t, call(t, base+"?node_cpu_info"))
	assert.Equal(t, "/metrics", last.Load())
	require.NoError(t, call(t, srv.URL+"/metrics/?name=node_cpu_info", tr("package", "==", "0")))
	assert.Equal(t, "/metrics", last.Load(), "no trailing slash, no name parameter")
	require.NoError(t, call(t, base+"?node_cpu_info", tr("package", "==", "1"), tr("core", "==", "0")))
	// all tests must hold for the same series: package 1 has no core 1
	err := call(t, base+"?node_cpu_info", tr("package", "==", "1"), tr("core", "==", "1"))
	assert.ErrorContains(t, err, `none of the 3 series of node_cpu_info has package == "1", core == "1"`)
	// a label the series lacks is ""
	require.NoError(t, call(t, base+"?node_cpu_info", tr("model_name", "==", ""), tr("package", "==", "0")))
	require.NoError(t, call(t, base+"?node_cpu_info", tr("package", "!=", "0")))
	require.NoError(t, call(t, base+"?node_cpu_info", tr("model_name", "=~", "EPYC")))
	assert.Error(t, call(t, base+"?node_cpu_info", tr("package", "==", "7")))
	assert.ErrorContains(t, call(t, base+"?nope"), "no metric named nope")
	assert.ErrorContains(t, call(t, base+"?node_cpu_infox", tr("package", "==", "0")), "none of the 1 series")
	assert.ErrorContains(t, call(t, srv.URL+"/other?node_cpu_info"), "404")
}

func TestPrometheusRetriesAndTimeout(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	fs, _ := spec.Lookup("prometheus")
	_, err := fs.Run(context.Background(), spec.Request{Target: srv.URL + "/metrics?node_load1", Timeout: time.Second, Retry: 2})
	require.NoError(t, err)
	assert.EqualValues(t, 3, hits.Load())

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	start := time.Now()
	_, err = fs.Run(context.Background(), spec.Request{Target: slow.URL + "/metrics?x", Timeout: 200 * time.Millisecond})
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}
