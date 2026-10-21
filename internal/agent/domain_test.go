package agent

import "testing"

func TestNormalizeDomainStrategy(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
		err   bool
	}{
		{"", "", false},
		{"use_ipv4", "UseIPv4", false},
		{"UseIPv4", "UseIPv4", false},
		{"use_ip", "UseIP", false},
		{"use_ipv6", "UseIPv6", false},
		{"as_is", "AsIs", false},
		{"nonsense", "", true},
	} {
		got, err := normalizeDomainStrategy(tc.value)
		if tc.err {
			if err == nil {
				t.Fatalf("normalizeDomainStrategy(%q) did not return an error", tc.value)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("normalizeDomainStrategy(%q) = %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
}
