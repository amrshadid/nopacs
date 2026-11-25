package storageswitch

// setBeforeLaunch sets the hook called after a switch is accepted in the
// database and before its run is started.
func (s *Switcher) setBeforeLaunch(hook func()) { s.seams.beforeLaunch = hook }
