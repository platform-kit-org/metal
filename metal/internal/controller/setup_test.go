package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetupAllRefusesNoLanes(t *testing.T) {
	assert.Error(t, SetupAll(nil, Options{}))
	assert.Error(t, SetupAll(nil, Options{}, laneCount))
}
