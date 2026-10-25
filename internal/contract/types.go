// Package contract holds the shared data types exchanged between dissident
// modules. It contains no behavior and imports only the standard library.
package contract

import "time"

// Protocol is a share-link protocol understood by dissident.
type Protocol string

const (
	ProtocolVLESS       Protocol = "vless"
	ProtocolVMess       Protocol = "vmess"
	ProtocolTrojan      Protocol = "trojan"
	ProtocolShadowsocks Protocol = "ss"
	ProtocolSOCKS5      Protocol = "socks5"
)

// Link is one share link after parsing. Params holds protocol-specific
// transport and security options in their canonical lowercase form.
type Link struct {
	Raw      string
	Hash     string
	Protocol Protocol
	Name     string
	Server   string
	Port     int
	Params   map[string]string
}

// EntryState is the lifecycle state of a catalog entry.
type EntryState string

const (
	StateCatalog     EntryState = "catalog"
	StateTesting     EntryState = "testing"
	StateReady       EntryState = "ready"
	StateActive      EntryState = "active"
	StateFailed      EntryState = "failed"
	StateUnsupported EntryState = "unsupported"
)

// Stage is one step of the qualification pipeline.
type Stage string

const (
	StageParse     Stage = "parse"
	StageDedupe    Stage = "dedupe"
	StageTCP       Stage = "tcp"
	StageTLS       Stage = "tls"
	StageProtocol  Stage = "protocol"
	StageRender    Stage = "render"
	StageReal      Stage = "real"
	StageContent   Stage = "content"
	StageSpeed     Stage = "speed"
	StageStability Stage = "stability"
)

// Result is the outcome of one stage test for one link.
type Result struct {
	Hash       string
	Stage      Stage
	OK         bool
	Reason     string
	At         time.Time
	TCPLatency time.Duration
	TTFB       time.Duration
	SpeedMbps  float64
	HTTPStatus int
	Bytes      int64
	ExitIP     string
}

// Entry is one catalog item plus its last known qualification result.
type Entry struct {
	Link     Link
	State    EntryState
	Score    float64
	Last     Result
	Attempts int
	Updated  time.Time
}

// Ready is a qualified standby link available for promotion.
type Ready struct {
	Link      Link
	Score     float64
	LastCheck time.Time
	ExitIP    string
}

// Active is the single WAN currently serving traffic.
type Active struct {
	Link      Link
	Since     time.Time
	ExitIP    string
	XrayPID   int
	LocalPort int
}

// Snapshot is the immutable runtime view served to the control plane.
type Snapshot struct {
	Generation     uint64
	Active         *Active
	Ready          []Ready
	CatalogCount   int
	SupportedCount int
	Testing        int
	Updated        time.Time
}

// AccessRecord is one proxy access-log record. Credentials must never be
// placed in these fields.
type AccessRecord struct {
	Time     time.Time
	Src      string
	State    string
	Dst      string
	Domain   string
	Port     int
	Proto    string
	WAN      string
	Up       int64
	Down     int64
	Duration time.Duration
	Reason   string
}
