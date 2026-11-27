package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
