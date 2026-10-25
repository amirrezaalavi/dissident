package xraycfg

import "encoding/json"

// Config is a complete xray configuration document. Dissident renders one
// SOCKS inbound and exactly one outbound: there is deliberately no direct or
// freedom fallback, so a broken outbound can never silently leak traffic.
type Config struct {
	Log       LogConfig  `json:"log"`
	Inbounds  []Inbound  `json:"inbounds"`
	Outbounds []Outbound `json:"outbounds"`
}

// LogConfig is the xray log block.
type LogConfig struct {
	LogLevel string `json:"loglevel"`
}

// Inbound is the local SOCKS listener xray exposes to dissident.
type Inbound struct {
	Listen   string          `json:"listen"`
	Port     int             `json:"port"`
	Protocol string          `json:"protocol"`
	Settings json.RawMessage `json:"settings"`
}

// Outbound is the single upstream rendered from a contract.Link. Mux is
// intentionally absent: the demo never multiplexes.
type Outbound struct {
	Protocol       string          `json:"protocol"`
	Settings       json.RawMessage `json:"settings"`
	StreamSettings *StreamSettings `json:"streamSettings,omitempty"`
}

// StreamSettings is the transport and security block.
type StreamSettings struct {
	Network         string           `json:"network"`
	Security        string           `json:"security"`
	TLSSettings     *TLSSettings     `json:"tlsSettings,omitempty"`
	RealitySettings *RealitySettings `json:"realitySettings,omitempty"`
	WSSettings      *WSSettings      `json:"wsSettings,omitempty"`
	GRPCSettings    *GRPCSettings    `json:"grpcSettings,omitempty"`
	TCPSettings     *TCPSettings     `json:"tcpSettings,omitempty"`
	XHTTPSettings   *XHTTPSettings   `json:"xhttpSettings,omitempty"`
}

// TLSSettings is emitted for security=tls.
type TLSSettings struct {
	ServerName    string   `json:"serverName,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	ALPN          []string `json:"alpn,omitempty"`
	AllowInsecure bool     `json:"allowInsecure,omitempty"`
}

// RealitySettings is emitted for security=reality (never together with
// TLSSettings).
type RealitySettings struct {
	ServerName  string `json:"serverName,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	PublicKey   string `json:"publicKey,omitempty"`
	ShortID     string `json:"shortId,omitempty"`
	SpiderX     string `json:"spiderX,omitempty"`
}

// WSSettings is emitted for network=ws.
type WSSettings struct {
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// GRPCSettings is emitted for network=grpc.
type GRPCSettings struct {
	ServiceName string `json:"serviceName,omitempty"`
}

// TCPSettings is emitted for network=tcp with a non-plain header.
type TCPSettings struct {
	Header *TCPHeader `json:"header,omitempty"`
}

// TCPHeader is the TCP obfuscation header.
type TCPHeader struct {
	Type    string       `json:"type"`
	Request *HTTPRequest `json:"request,omitempty"`
}

// HTTPRequest is the fake HTTP request used by header type http.
type HTTPRequest struct {
	Version string              `json:"version,omitempty"`
	Method  string              `json:"method,omitempty"`
	Path    []string            `json:"path,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
}

// XHTTPSettings is emitted for network=xhttp.
type XHTTPSettings struct {
	Path  string          `json:"path"`
	Host  string          `json:"host"`
	Mode  string          `json:"mode"`
	Extra json.RawMessage `json:"extra,omitempty"`
}
