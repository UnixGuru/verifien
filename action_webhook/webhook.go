// Package actionwebhook is the webhook action: it POSTs the failure report, as
// JSON, to a URL (Slack-style incoming webhooks, PagerDuty, your own service).
package actionwebhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"verifien/spec"
)

func init() {
	spec.RegisterAction("webhook", spec.ActionSpec{
		Run:      run,
		Options:  map[string]spec.Field{"url": {Kind: spec.KString}},
		Settings: map[string]spec.Field{"webhook_url": {Kind: spec.KString}},
		Check: func(req spec.ActionRequest) error {
			_, err := target(req)
			return err
		},
	})
}

// target is the URL to post to: the action's own {url "..."}, else webhook_url
// from [DEFAULTS].
func target(req spec.ActionRequest) (string, error) {
	u := req.Opt("url")
	if u == "" {
		u = req.Setting("webhook_url")
	}
	if u == "" {
		return "", errors.New(`no url: give {url "https://..."} or set webhook_url in [DEFAULTS]`)
	}
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("url %q must be an http:// or https:// address", u)
	}
	return u, nil
}

func run(ctx context.Context, req spec.ActionRequest) error {
	u, err := target(req)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req.Report)
	if err != nil {
		return err
	}
	return req.Attempt(ctx, func(actx context.Context) error {
		hr, err := http.NewRequestWithContext(actx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		hr.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(hr)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("webhook: %s answered %s", u, resp.Status)
		}
		return nil
	})
}
