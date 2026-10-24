// Package actionstdout is the stdout action: it writes the JSON report of the
// section to standard output, one line per section.
package actionstdout

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"verifien/spec"
)

// Out is where the report goes. Tests replace it.
var Out io.Writer = os.Stdout

func init() {
	spec.RegisterAction("stdout", spec.ActionSpec{Run: run})
}

func run(ctx context.Context, req spec.ActionRequest) error {
	b, err := json.Marshal(req.Report) // compact: one line
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// One Write, so a line is never split by another action; Attempt because a
	// pipe nobody reads can block.
	return req.Attempt(ctx, func(context.Context) error {
		_, err := Out.Write(b)
		return err
	})
}
