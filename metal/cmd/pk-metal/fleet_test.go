package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuardTimeout(t *testing.T) {
	assert.Equal(t, 90*time.Second, guardTimeout(90*time.Second, true))
	// On the command line 0 is "never"; the fleet option reads 0 as the
	// default, so the flag's 0 must become a negative value.
	assert.Negative(t, guardTimeout(0, true))
	assert.Negative(t, guardTimeout(-time.Second, true))
	// A flag not given leaves the choice to the driver, which waits longer
	// for a machine under tcg.
	assert.Zero(t, guardTimeout(90*time.Second, false))
}

// TestHangTimeoutHelpShowsNoDefault: the default of --hang-timeout depends
// on the accelerator, and the usage text gives both values. The help must
// not add one fixed default after them.
func TestHangTimeoutHelpShowsNoDefault(t *testing.T) {
	fs := captureFlags(simFleetCmd, nil)
	require.NotNil(t, fs)
	var help strings.Builder
	fs.SetOutput(&help)
	fs.PrintDefaults()
	_, flag, ok := strings.Cut(help.String(), "-hang-timeout")
	require.True(t, ok)
	flag, _, _ = strings.Cut(flag, "\n  -")
	assert.NotContains(t, flag, "(default")
}
