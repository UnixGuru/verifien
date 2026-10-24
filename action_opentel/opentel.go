// Package actionopentel is the opentel action: it sends one error-level log
// record per failed ck to an OpenTelemetry collector.
//
// It speaks OTLP/HTTP with JSON bodies (POST <endpoint>/v1/logs) directly, so it
// needs no OpenTelemetry library. gRPC and protobuf bodies are not supported.
//
// Configuration, first match wins:
//
//	endpoint  otel_endpoint in [DEFAULTS]
//	          OTEL_EXPORTER_OTLP_LOGS_ENDPOINT (used as given)
//	          OTEL_EXPORTER_OTLP_ENDPOINT      (/v1/logs is added)
//	service   otel_service in [DEFAULTS], OTEL_SERVICE_NAME, else "verifien"
//	headers   OTEL_EXPORTER_OTLP_HEADERS and OTEL_EXPORTER_OTLP_LOGS_HEADERS
//	          ("k=v,k2=v2"). Tokens belong in the environment, not the config file.
package actionopentel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"verifien/spec"
)

func init() {
	spec.RegisterAction("opentel", spec.ActionSpec{
		Run: run,
		Settings: map[string]spec.Field{
			"otel_endpoint": {Kind: spec.KString}, // http://collector:4318
			"otel_service":  {Kind: spec.KString}, // default verifien
		},
		Check: func(req spec.ActionRequest) error {
			_, err := endpoint(req)
			return err
		},
	})
}

func withPath(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1/logs") {
		return base
	}
	return base + "/v1/logs"
}

func endpoint(req spec.ActionRequest) (string, error) {
	var u string
	switch {
	case req.Setting("otel_endpoint") != "":
		u = withPath(req.Setting("otel_endpoint"))
	case os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != "":
		u = os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")
	case os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "":
		u = withPath(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	default:
		return "", errors.New("no endpoint: set otel_endpoint in [DEFAULTS] or OTEL_EXPORTER_OTLP_ENDPOINT in the environment")
	}
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("endpoint %q must be an http:// or https:// address (OTLP/HTTP; gRPC is not supported)", u)
	}
	return u, nil
}

func service(req spec.ActionRequest) string {
	if v := req.Setting("otel_service"); v != "" {
		return v
	}
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		return v
	}
	return "verifien"
}

// headers reads the standard "k=v,k2=v2" environment variables.
func headers() map[string]string {
	h := map[string]string{}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS"} {
		for _, pair := range strings.Split(os.Getenv(name), ",") {
			k, v, ok := strings.Cut(pair, "=")
			k = strings.TrimSpace(k)
			if !ok || k == "" {
				continue
			}
			v = strings.TrimSpace(v)
			if dec, err := url.PathUnescape(v); err == nil {
				v = dec
			}
			h[k] = v
		}
	}
	return h
}

type obj = map[string]any

func attr(key, val string) obj {
	return obj{"key": key, "value": obj{"stringValue": val}}
}

// payload builds an OTLP logs request: one record per failed ck.
func payload(req spec.ActionRequest) obj {
	now := strconv.FormatInt(time.Now().UnixNano(), 10) // 64-bit ints are strings in OTLP JSON
	records := make([]obj, 0, len(req.Report.Failures))
	for _, f := range req.Report.Failures {
		records = append(records, obj{
			"timeUnixNano":   now,
			"severityNumber": 17, // ERROR
			"severityText":   "ERROR",
			"body":           obj{"stringValue": f.Message},
			"attributes": []obj{
				attr("verifien.section", req.Report.Section),
				attr("verifien.ck", f.Ck),
			},
		})
	}
	return obj{"resourceLogs": []obj{{
		"resource":  obj{"attributes": []obj{attr("service.name", service(req))}},
		"scopeLogs": []obj{{"scope": obj{"name": "verifien"}, "logRecords": records}},
	}}}
}

func run(ctx context.Context, req spec.ActionRequest) error {
	u, err := endpoint(req)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload(req))
	if err != nil {
		return err
	}
	hdr := headers()
	return req.Attempt(ctx, func(actx context.Context) error {
		hr, err := http.NewRequestWithContext(actx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		for k, v := range hdr {
			hr.Header.Set(k, v)
		}
		hr.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(hr)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("opentel: %s answered %s", u, resp.Status)
		}
		return nil
	})
}
