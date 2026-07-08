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
