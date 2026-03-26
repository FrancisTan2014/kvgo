package server

import "time"

// ---------------------------------------------------------------------------
// Network
// ---------------------------------------------------------------------------

const (
	defaultHost    = "127.0.0.1" // localhost only; override with Options.Host
	defaultNetwork = NetworkTCP
)

// Supported network types for Options.Network.
const (
	NetworkTCP  = "tcp"
	NetworkTCP4 = "tcp4"
	NetworkTCP6 = "tcp6"
	NetworkUnix = "unix"
)

var supportedNetworks = map[string]bool{
	NetworkTCP:  true,
	NetworkTCP4: true,
	NetworkTCP6: true,
	NetworkUnix: true,
}

// ---------------------------------------------------------------------------
// Options defaults
// ---------------------------------------------------------------------------

const (
	defaultReadTimeout  = 5 * time.Second
	defaultWriteTimeout = 5 * time.Second
	defaultMaxFrameSize = 16 << 20 // 16MB
)

// applyDefaults fills zero-valued fields with sensible defaults.
func (o *Options) applyDefaults() {
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = defaultReadTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	if o.MaxFrameSize <= 0 {
		o.MaxFrameSize = defaultMaxFrameSize
	}
}
