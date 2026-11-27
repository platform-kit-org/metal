package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// check shows the slots that the operator takes for an install and the boot
// target of each system, and says when no install can work.
func TestCheckShowsTheSlotsOfAnInstall(t *testing.T) {
	for _, c := range []struct {
		name    string
		fixture string
		edit    func(*sim.Fixture)
		lines   []string
		why     string
	}{
		{name: "two slots", fixture: "generic",
			lines: []string{"slots:    boot=2 config=1\n", "system:   1 serial=GEN0000001 power=Off boot=Cd\n"}},
		{name: "one slot", fixture: "huawei-2288h-v5",
			lines: []string{"slots:    boot=CD config=-\n", "boot=Cd\n"}},
		{name: "no removable boot target", fixture: "generic",
			edit:  func(f *sim.Fixture) { f.Systems[0].BootTargets = []string{"Pxe", "Hdd"} },
			lines: []string{"slots:    boot=2 config=1\n", "boot=-\n"}, why: "no system offers Cd or UsbCd as a boot target"},
		{name: "no slot can mount an image", fixture: "huawei-2288h-v5",
			edit:  func(f *sim.Fixture) { f.VirtualMedia[0].OemActions = nil },
			lines: []string{"slots:    boot=- config=-\n"}, why: "no slot can mount an image"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, err := sim.LoadFixture(c.fixture)
			require.NoError(t, err)
			if c.edit != nil {
				c.edit(f)
			}
			var out bytes.Buffer
			why, err := check(t.Context(), sim.NewState(f), &out)
			require.NoError(t, err)
			for _, l := range c.lines {
				assert.Contains(t, out.String(), l)
			}
			assert.Equal(t, c.why, why)
		})
	}
}
