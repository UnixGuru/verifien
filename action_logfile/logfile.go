// Package actionlogfile is the logfile action: it appends one JSON line per
// failed ck to a file.
package actionlogfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"

	"verifien/spec"
)

func init() {
	spec.RegisterAction("logfile", spec.ActionSpec{
		Run:      run,
		Options:  map[string]spec.Field{"path": {Kind: spec.KString}},
		Settings: map[string]spec.Field{"logfile_path": {Kind: spec.KString}},
		Check: func(req spec.ActionRequest) error {
			_, err := target(req)
			return err
		},
	})
}

// target is the file to append to: the action's own {path "..."}, else
// logfile_path from [DEFAULTS].
func target(req spec.ActionRequest) (string, error) {
	p := req.Opt("path")
	if p == "" {
		p = req.Setting("logfile_path")
	}
	if p == "" {
		return "", errors.New(`no path: give {path "/var/log/verifien.log"} or set logfile_path in [DEFAULTS]`)
	}
	return p, nil
}

type line struct {
	Time    string `json:"time"`
	Program string `json:"program"`
	Section string `json:"section"`
	Ck      string `json:"ck"`
	Message string `json:"message"`
}

func run(ctx context.Context, req spec.ActionRequest) error {
	path, err := target(req)
	if err != nil {
		return err
	}
	// Build everything first and write it with one call, so a retry after a
	// failed write cannot leave half a report behind and then repeat it.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // adds the newline
	for _, f := range req.Report.Failures {
		if err := enc.Encode(line{req.Report.Time, req.Report.Program, req.Report.Section, f.Ck, f.Message}); err != nil {
			return err
		}
	}
	// A file on a hung network mount can block, hence req.Attempt.
	return req.Attempt(ctx, func(context.Context) error {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		if _, err := f.Write(buf.Bytes()); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}
