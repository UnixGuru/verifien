package env

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"verifien/spec"
)

func TestEnvironSetAndValue(t *testing.T) {
	t.Setenv("VERIFIEN_TEST_VAR", "hello")
	start := time.Now()
	res, err := environ(context.Background(), spec.Request{Target: "VERIFIEN_TEST_VAR"})
	require.NoError(t, err)
	assert.Equal(t, "yes", res["set"])
	assert.Equal(t, "hello", res["value"])
	assert.Less(t, time.Since(start), 50*time.Millisecond, "environ should not wait for anything")
}

func TestEnvironSetButEmptyIsNotUnset(t *testing.T) {
	t.Setenv("VERIFIEN_TEST_EMPTY", "")
	res, _ := environ(context.Background(), spec.Request{Target: "VERIFIEN_TEST_EMPTY"})
	assert.Equal(t, "yes", res["set"])
	assert.Equal(t, "", res["value"])
}

func TestEnvironUnset(t *testing.T) {
	res, err := environ(context.Background(), spec.Request{Target: "VERIFIEN_SURELY_NOT_SET_XYZ"})
	require.NoError(t, err)
	assert.Equal(t, "no", res["set"])
	assert.Equal(t, "", res["value"])
}

func TestEnvironEmptyNameIsAnError(t *testing.T) {
	_, err := environ(context.Background(), spec.Request{})
	assert.Error(t, err, "want an error for an empty variable name")
}

// environ accepts the standard options (timeout, retry, sleep) but ignores them.
func TestEnvironAcceptsAndIgnoresStandardOptions(t *testing.T) {
	fs, ok := spec.Lookup("environ")
	require.True(t, ok, "environ is not registered")
	for _, opt := range []string{"timeout", "retry", "sleep", "msg"} {
		assert.Contains(t, fs.Options, opt, "option %q should be accepted", opt)
	}
	start := time.Now()
	environ(context.Background(), spec.Request{Target: "NOPE_XYZ", Retry: 5, Sleep: 2 * time.Second, Timeout: time.Second})
	assert.Less(t, time.Since(start), 100*time.Millisecond, "retry/sleep were not ignored")
}
