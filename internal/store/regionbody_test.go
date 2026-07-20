package store

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
)

// regionbody_test.go — the hermetic net for the store FIELD-DROP trap on the region_defs path (#356).
//
// A region persists through a JSONB body with an EXPLICIT field list on both ends: regionBody's struct,
// the marshal in import.go, and the unmarshal in content.go. Add a field to content.RegionDTO and forget
// any one of those three and the failure is silent and shaped to evade testing: the YAML pack and the
// embedded fixture round-trip perfectly, every unit test passes, and only a POSTGRES-sourced pack — i.e.
// staging and production — loads the region with the field empty, with no error logged anywhere.
//
// That exact failure has landed in this repo three times (rounds 11, 35, 36). The Postgres round-trip
// test that proves the whole path is gated on TELOS_TEST_DSN and SKIPS on a developer machine, which is
// how it keeps getting through. This test is hermetic on purpose: it runs everywhere, including in the
// pre-commit gate, and it fails the moment a DTO field has no persisted counterpart.

// TestRegionBodyCoversEveryPersistedRegionDTOField fails when content.RegionDTO grows a field that
// regionBody does not carry. Ref is excluded because it is the relational PK column, not body content.
func TestRegionBodyCoversEveryPersistedRegionDTOField(t *testing.T) {
	// Ref is the row's PK; everything else must ride the JSONB body.
	notInBody := map[string]bool{"Ref": true}

	bodyFields := map[string]bool{}
	bt := reflect.TypeOf(regionBody{})
	for i := 0; i < bt.NumField(); i++ {
		bodyFields[bt.Field(i).Name] = true
	}

	dt := reflect.TypeOf(content.RegionDTO{})
	for i := 0; i < dt.NumField(); i++ {
		name := dt.Field(i).Name
		if notInBody[name] {
			continue
		}
		assert.Truef(t, bodyFields[name],
			"content.RegionDTO.%s has no matching field on store.regionBody, so it will round-trip through a "+
				"YAML pack and silently VANISH through a Postgres one (staging/prod load it empty, no error). "+
				"Add it to regionBody AND to the marshal in import.go AND to the unmarshal in content.go.",
			name)
	}
}

// TestRegionBodyRoundTripsEveryField is the value-level half: the struct having a field proves nothing if
// the marshal or unmarshal site forgot it. This drives the SAME json encode/decode the store uses and
// asserts every field survives with a distinctive value.
func TestRegionBodyRoundTripsEveryField(t *testing.T) {
	rg := content.RegionDTO{
		Ref:    "heartlands",
		Name:   "The Heartlands",
		Zones:  []string{"midgaard", "darkwood"},
		Script: "function on_signal(e, p) director.set('last', e) end",
	}

	// The REAL encode/decode the import and load paths call — not a restatement of them. Restating the
	// field list here would make this test pass by construction however the store actually behaved, which
	// is precisely the failure mode it exists to catch.
	body, err := marshalRegionBody(rg)
	require.NoError(t, err)

	got := content.RegionDTO{Ref: rg.Ref}
	require.NoError(t, applyRegionBody(&got, body))

	assert.Equal(t, rg, got,
		"a region must survive the JSONB body round-trip with every field intact — a mismatch here is the "+
			"field-drop trap, and it would only show up on a Postgres-sourced pack")
}

// capturingTx is a pgx.Tx that records every Exec instead of running it. Embedding the interface means any
// method these tests do not drive panics loudly rather than silently returning a zero value.
type capturingTx struct {
	pgx.Tx
	execs []capturedExec
}

type capturedExec struct {
	sql  string
	args []any
}

func (c *capturingTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.execs = append(c.execs, capturedExec{sql: sql, args: args})
	return pgconn.CommandTag{}, nil
}

// TestImportWritesEveryRegionFieldToTheRow drives the REAL import call site (insertGlobalDefs) and inspects
// the bytes it hands Postgres.
//
// This is the assertion the helper-level round-trip test cannot make. Collapsing the field list into
// marshalRegionBody/applyRegionBody removed the three-places-must-agree hazard but introduced a quieter
// one: a test that calls the helper directly proves the HELPER is complete, not that the import and load
// paths still call it. Reverting import.go to an inline `json.Marshal(regionBody{Name: rg.Name, Zones:
// rg.Zones})` — the exact shape it had before this change, and the exact shape of the field-drop bug that
// has landed here three times — leaves every other test in the package green. It does not leave this one
// green.
func TestImportWritesEveryRegionFieldToTheRow(t *testing.T) {
	rg := content.RegionDTO{
		Ref:    "heartlands",
		Name:   "The Heartlands",
		Zones:  []string{"midgaard", "darkwood"},
		Script: "function on_signal(e, p) director.set('last', e) end",
	}
	tx := &capturingTx{}
	require.NoError(t, insertGlobalDefs(context.Background(), tx, content.Pack{
		Pack:    "demo",
		Regions: []content.RegionDTO{rg},
	}))

	var body []byte
	for _, e := range tx.execs {
		if strings.Contains(e.sql, "INSERT INTO region_defs") {
			require.Len(t, e.args, 3, "region_defs takes (ref, pack, body)")
			require.Equal(t, rg.Ref, e.args[0])
			b, ok := e.args[2].([]byte)
			require.True(t, ok, "the region body must be marshalled JSON bytes")
			body = b
		}
	}
	require.NotNil(t, body, "the import path must have written a region_defs row")

	// Decode what the IMPORT actually produced, not what a helper called from the test produces.
	got := content.RegionDTO{Ref: rg.Ref}
	require.NoError(t, applyRegionBody(&got, body))
	assert.Equal(t, rg, got,
		"the region_defs row the import path writes must carry EVERY RegionDTO field. A field missing here "+
			"round-trips fine through a YAML pack and vanishes through a Postgres one — staging and prod load "+
			"the region with it empty and nothing logs an error.")
}

// TestRegionScriptSurvivesPostgresRoundTrip is the gated end-to-end half — the test content.go's regionBody
// comment names, and the only one that covers the LOAD side of the seam.
//
// The hermetic tests above pin the helpers and the import call site, but loadGlobalDefs reads through a live
// pool and cannot be faked: reverting its applyRegionBody call to an inline unmarshal that drops Script
// leaves every hermetic test in this package green while staging and prod load every region with an EMPTY
// script — the region silently runs with no orchestration at all, and nothing logs an error. This is the
// exact class of failure that has landed in this repo three times, and it is only visible with a database.
//
// Gated on TELOS_TEST_DSN (skipped on a bare dev machine, RUN in CI's `make test-integration`).
func TestRegionScriptSurvivesPostgresRoundTrip(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	pack := "regionrt-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	rg := content.RegionDTO{
		Ref:    pack + "-heartlands",
		Name:   "The Heartlands",
		Zones:  []string{"midgaard", "darkwood"},
		Script: "function on_signal(e, p) director.set('last', e) end",
	}
	require.NoError(t, p.ImportPacks(ctx, []content.Pack{{Pack: pack, Regions: []content.RegionDTO{rg}}}))
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM region_defs WHERE pack = $1`, pack)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM pack_meta WHERE pack = $1`, pack)
	})

	lc, err := content.Load(ctx, p, []string{pack})
	require.NoError(t, err)

	var got *content.RegionDTO
	for i := range lc.Regions {
		if lc.Regions[i].Ref == rg.Ref {
			got = &lc.Regions[i]
		}
	}
	require.NotNil(t, got, "the imported region must load back")
	assert.Equal(t, rg, *got,
		"a region must survive a real Postgres import+load with EVERY field intact — a dropped Script here "+
			"means every region in staging and prod runs with no director orchestration, silently")
}
