package bmc_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

func TestMediaSlots(t *testing.T) {
	insert := func(media ...bmc.VirtualMedia) bmc.Capabilities {
		caps := bmc.Capabilities{MediaDialect: map[string]bmc.MediaDialect{}}
		for _, m := range media {
			caps.MediaDialect[m.ID] = bmc.DialectStandard
		}
		return caps
	}
	slot := func(id string, types ...string) bmc.VirtualMedia { return bmc.VirtualMedia{ID: id, MediaTypes: types} }
	for _, c := range []struct {
		name         string
		media        []bmc.VirtualMedia
		boot, config string
	}{
		{"USB stick listed first: the installer takes the optical slot", []bmc.VirtualMedia{slot("1", "USBStick"), slot("2", "CD", "DVD")}, "2", "1"},
		{"two optical", []bmc.VirtualMedia{slot("CD1", "CD"), slot("CD2", "CD")}, "CD1", "CD2"},
		{"one optical only", []bmc.VirtualMedia{slot("CD", "CD")}, "CD", ""},
		{"an untyped slot boots, a USB stick takes the config", []bmc.VirtualMedia{slot("USBStick", "USBStick"), slot("CD")}, "CD", "USBStick"},
		{"USB sticks only", []bmc.VirtualMedia{slot("U1", "USBStick"), slot("U2", "USBStick")}, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, boot, config := bmc.MediaSlots(c.media, insert(c.media...))
			assert.Equal(t, c.boot, boot)
			assert.Equal(t, c.config, config)
		})
	}

	// A slot without an insert mechanism is not a slot of the install.
	media := []bmc.VirtualMedia{slot("CD"), slot("USBStick", "USBStick")}
	caps := bmc.Capabilities{MediaDialect: map[string]bmc.MediaDialect{"CD": bmc.DialectVmmControl, "USBStick": bmc.DialectNone}}
	insertable, boot, config := bmc.MediaSlots(media, caps)
	assert.Equal(t, []string{"CD"}, insertable)
	assert.Equal(t, "CD", boot)
	assert.Empty(t, config, "a one-slot BMC")
}
