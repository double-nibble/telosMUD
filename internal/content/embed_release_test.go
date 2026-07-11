//go:build nofixture

package content

import "testing"

// Verifies the release embed (build tag `nofixture`): the bootstrap `core` pack is present, the `demo`
// fixture is NOT. Run with `go test -tags nofixture ./internal/content/`.
func TestReleaseEmbedStripsDemo(t *testing.T) {
	if _, found, err := LoadPack(CorePack); err != nil || !found {
		t.Fatalf("core pack must stay embedded in a release build: found=%v err=%v", found, err)
	}
	if _, found, err := LoadPack(DemoPack); err != nil {
		t.Fatalf("LoadPack(demo) should be a clean not-found, got err=%v", err)
	} else if found {
		t.Fatal("the demo FIXTURE must NOT be embedded in a release (nofixture) build")
	}
}
