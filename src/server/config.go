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
// Replication
// ---------------------------------------------------------------------------

const (
	replicaSendBuffer = 1024 // max queued writes per replica
	heartbeatInterval = 200 * time.Millisecond
	retryInterval     = 100 * time.Millisecond
)

// ---------------------------------------------------------------------------
// Election
// ---------------------------------------------------------------------------

const electionTimeout = 10 * heartbeatInterval

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

const reconcileInterval = 3 * time.Second

// ---------------------------------------------------------------------------
// Backlog
// ---------------------------------------------------------------------------

const (
	DefaultBacklogSizeLimit    = 16 * 1024 * 1024
	DefaultBacklogTrimDuration = 100 * time.Millisecond
	MinBacklogSize             = 1 * 1024 * 1024
	TrimRatioThreshold         = 2
)

// ---------------------------------------------------------------------------
// Options defaults
// ---------------------------------------------------------------------------

const (
	defaultReadTimeout         = 5 * time.Second
	defaultWriteTimeout        = 5 * time.Second
	defaultMaxFrameSize        = 16 << 20 // 16MB
	defaultBacklogSizeLimit    = 16 << 20 // 16MB
	defaultBacklogTrimDuration = 100 * time.Millisecond
	defaultQuorumWriteTimeout  = 500 * time.Millisecond
	defaultQuorumReadTimeout   = 500 * time.Millisecond
	defaultStaleHeartbeat      = 1 * time.Second
	defaultStaleLag            = 1000
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
	if o.BacklogSizeLimit <= 0 {
		o.BacklogSizeLimit = defaultBacklogSizeLimit
	}
	if o.BacklogTrimDuration <= 0 {
		o.BacklogTrimDuration = defaultBacklogTrimDuration
	}
	if o.QuorumWriteTimeout <= 0 {
		o.QuorumWriteTimeout = defaultQuorumWriteTimeout
	}
	if o.QuorumReadTimeout <= 0 {
		o.QuorumReadTimeout = defaultQuorumReadTimeout
	}
	if o.ReplicaStaleHeartbeat <= 0 {
		o.ReplicaStaleHeartbeat = defaultStaleHeartbeat
	}
	if o.ReplicaStaleLag <= 0 {
		o.ReplicaStaleLag = defaultStaleLag
	}
}
