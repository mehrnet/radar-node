package agent

import (
	"fmt"
	"strings"
)

// normalizeDomainStrategy maps the agent's CLI spelling to Xray's own
// domainStrategy values. poolconfig and jobprobe read the exported
// RADAR_DOMAIN_STRATEGY value, so the normalized form is what child
// processes see.
func normalizeDomainStrategy(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return "", nil
	case "as_is", "asis":
		return "AsIs", nil
	case "use_ipv4", "useipv4":
		return "UseIPv4", nil
	case "use_ip", "useip":
		return "UseIP", nil
	case "use_ipv6", "useipv6":
		return "UseIPv6", nil
	default:
		return "", fmt.Errorf("--domain-strategy must be one of as_is, use_ipv4, use_ip, use_ipv6 (got %q)", value)
	}
}
