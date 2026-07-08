package content

import (
	"reflect"
	"testing"
)

// TestResolveEnabledPacks pins the shared pack-resolution precedence (#246) that BOTH telos-world and
// telos-account must use identically — an explicit override wins, else the registry set, else the demo pack.
func TestResolveEnabledPacks(t *testing.T) {
	tests := []struct {
		name     string
		explicit []string
		registry []string
		want     []string
	}{
		{"explicit override wins over registry", []string{"opsA", "opsB"}, []string{"reg"}, []string{"opsA", "opsB"}},
		{"registry when no override", nil, []string{"midgaard", "darkwood"}, []string{"midgaard", "darkwood"}},
		{"demo when neither", nil, nil, []string{DemoPack}},
		{"empty (non-nil) slices fall through like nil", []string{}, []string{}, []string{DemoPack}},
		{"explicit override wins even with an empty registry", []string{"only"}, nil, []string{"only"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveEnabledPacks(tc.explicit, tc.registry); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ResolveEnabledPacks(%v, %v) = %v, want %v", tc.explicit, tc.registry, got, tc.want)
			}
		})
	}
}

// TestCheckPackSetConsistency pins the #259 runtime divergence guard: an EXPLICIT override that disagrees with
// a NON-EMPTY published registry (in value OR order) is a divergence; no override, an empty/never-pulled
// registry, or an exact match is fine. Both telos-world and telos-account run this against their OWN override.
func TestCheckPackSetConsistency(t *testing.T) {
	tests := []struct {
		name        string
		explicit    []string
		registry    []string
		wantDiverge bool
	}{
		{"no override follows registry (consistent)", nil, []string{"midgaard", "darkwood"}, false},
		{"empty override follows registry (consistent)", []string{}, []string{"midgaard"}, false},
		{"fresh DB, override is bootstrap path (no published set)", []string{"demo"}, nil, false},
		{"override exactly matches published set", []string{"a", "b"}, []string{"a", "b"}, false},
		{"override omits a published pack (divergence)", []string{"a"}, []string{"a", "b"}, true},
		{"override adds a pack over the published set (divergence)", []string{"a", "b", "c"}, []string{"a", "b"}, true},
		{"override reorders the published set (divergence — load order is significant)", []string{"b", "a"}, []string{"a", "b"}, true},
		{"override names a wholly different set (divergence)", []string{"x"}, []string{"y"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPackSetConsistency(tc.explicit, tc.registry)
			if tc.wantDiverge && err == nil {
				t.Fatalf("CheckPackSetConsistency(%v, %v) = nil, want a divergence error", tc.explicit, tc.registry)
			}
			if !tc.wantDiverge && err != nil {
				t.Fatalf("CheckPackSetConsistency(%v, %v) = %v, want nil", tc.explicit, tc.registry, err)
			}
		})
	}
}
