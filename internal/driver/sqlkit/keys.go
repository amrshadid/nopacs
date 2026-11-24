package sqlkit

import (
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// ReadNetworks reads a key's networks as both drivers store them: the one
// written form of settings.ParseNetworks, ” for none. Every value is
// checked before it is stored (master.KeyRules.Validate), so one that does
// not follow the rule now is the database's fault, never the caller's, and
// a key read with it is refused rather than allowed from everywhere.
func ReadNetworks(stored string) (settings.Networks, error) {
	n, err := settings.ParseNetworks(stored)
	if err != nil {
		return nil, fault.New(fault.Internal, "a key's stored networks do not follow their rule")
	}
	return n, nil
}
