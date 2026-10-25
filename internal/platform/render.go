package platform

import (
	"encoding/json"
	"fmt"

	"dissident/internal/contract"
	"dissident/internal/xraycfg"
)

// RenderWithEgress renders link exactly as xraycfg.Render does. When
// egressProxy is empty the rendered bytes are returned unchanged. Otherwise
// an HTTP outbound tagged "egress" is appended and the first outbound is
// pointed at it with streamSettings.sockopt.dialerProxy. All original fields
// are preserved and the result must decode as JSON.
func RenderWithEgress(link contract.Link, port int, egressProxy string) ([]byte, error) {
	cfg, err := xraycfg.Render(link, port)
	if err != nil {
		return nil, err
	}
	if egressProxy == "" {
		return cfg, nil
	}
	proxy, err := parseEgressProxy(egressProxy)
	if err != nil {
		return nil, err
	}

	var document map[string]any
	if err := json.Unmarshal(cfg, &document); err != nil {
		return nil, fmt.Errorf("platform: render with egress: decode rendered config: %w", err)
	}
	rawOutbounds, ok := document["outbounds"]
	if !ok {
		return nil, fmt.Errorf("platform: render with egress: rendered config has no outbounds")
	}
	outbounds, ok := rawOutbounds.([]any)
	if !ok || len(outbounds) == 0 {
		return nil, fmt.Errorf("platform: render with egress: rendered config has no usable outbounds")
	}
	first, ok := outbounds[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("platform: render with egress: first outbound is not an object")
	}

	stream, err := childObject(first, "streamSettings")
	if err != nil {
		return nil, err
	}
	sockopt, err := childObject(stream, "sockopt")
	if err != nil {
		return nil, err
	}
	sockopt["dialerProxy"] = "egress"

	server := map[string]any{
		"address": proxy.host,
		"port":    proxy.port,
	}
	if proxy.hasAuth {
		server["users"] = []any{map[string]any{
			"user": proxy.user,
			"pass": proxy.pass,
		}}
	}
	outbounds = append(outbounds, map[string]any{
		"protocol": "http",
		"tag":      "egress",
		"settings": map[string]any{
			"servers": []any{server},
		},
	})
	document["outbounds"] = outbounds

	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("platform: render with egress: marshal config: %w", err)
	}
	var check any
	if err := json.Unmarshal(data, &check); err != nil {
		return nil, fmt.Errorf("platform: render with egress: rendered config does not parse: %w", err)
	}
	return data, nil
}

// childObject returns parent[key] as an object, creating it when absent. A
// present but non-object value is an error so malformed configs are not
// silently rewritten.
func childObject(parent map[string]any, key string) (map[string]any, error) {
	value, ok := parent[key]
	if !ok || value == nil {
		child := map[string]any{}
		parent[key] = child
		return child, nil
	}
	child, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("platform: render with egress: %s section is not an object", key)
	}
	return child, nil
}
