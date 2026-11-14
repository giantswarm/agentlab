package config

// BandwidthShaperOn says whether the lab's `up` and `platform` ensure the
// bandwidth shaper on the shared registry mirror: off unless the lab turned
// it on.
func (c *Config) BandwidthShaperOn() bool {
	return c.BandwidthShaper != nil && *c.BandwidthShaper
}

// SetBandwidthShaper records the lab's choice, either way, so a later
// default leaves it as it is.
func (c *Config) SetBandwidthShaper(on bool) {
	c.BandwidthShaper = new(on)
}
