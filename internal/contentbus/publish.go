package contentbus

import (
	"context"

	"github.com/double-nibble/telosmud/internal/content"
)

// publish.go is the WRITER side trigger (docs/PHASE4-PLAN.md §5): after a content write (a `make
// seed` re-import, a future OLC save), the writer publishes an invalidation for each changed ref
// so every running shard hot-reloads it. v1 publishes EVERY room/item/mob ref in the imported pack
// (a re-seed replaces the whole pack, so every ref may have changed); a finer changed-ref diff is
// a later refinement — over-publishing is harmless (each shard re-reads and swaps an identical
// prototype if nothing changed).

// PublishPack publishes an invalidation for every room, item, and mob prototype in pk on bus, plus a
// zone-SHAPE invalidation per zone (which drives the live-room reconcile — add/update/remove, #191). It
// is the seed/OLC trigger: a running shard subscribed to the bus re-reads and swaps each prototype, then
// reconciles each zone's room graph against the desired state the KindZone invalidation carries. Errors
// from individual publishes are returned at the first failure so the caller can log; a partial publish
// still hot-reloads the refs that made it onto the wire.
//
// version is the AUTHORITATIVE monotonic content version this call carries, supplied by the caller: the
// PG-minted content_version for a coordinated pull (#212 slice 4), or a wall-clock-nanos stamp for a
// shard-local `reload` / dev seed. Every invalidation of one call shares it, so a zone's reconcile can
// drop a STALE reconcile a racing reload reordered ahead of a newer one (the guard is last-writer-wins by
// this value, not by arrival order). Both sources are nanos-SCALE so they never wedge the guard; the
// PG-minted value is GREATEST(prev+1, now_nanos) from the PULLER host's clock, while a shard-local reload
// stamps THIS shard's clock — so a reload racing a pull can be superseded when the shard's clock lags the
// puller's (a CROSS-HOST clock-skew window, not elapsed time; only the zone-shape reconcile is dropped,
// per-ref data still applies — documented at the reload call site). The KindZone
// invalidation is emitted LAST per zone and carries that zone's full room-ref set + start room, so the
// reconcile converges off the already-swapped cache with no source re-read (the contentbus is a single
// ordered subject — the per-ref cache swaps are delivered before the trailing KindZone reconcile).
func PublishPack(ctx context.Context, bus Bus, pk content.Pack, version uint64) (int, error) {
	if bus == nil {
		return 0, nil
	}
	n := 0
	pub := func(kind, ref string) error {
		if err := bus.Publish(ctx, Invalidation{Kind: kind, Ref: ref, Pack: pk.Pack, Version: version}); err != nil {
			return err
		}
		n++
		return nil
	}
	for _, z := range pk.Zones {
		for _, r := range z.Rooms {
			if err := pub(content.KindRoom, r.Ref); err != nil {
				return n, err
			}
		}
		for _, it := range z.Items {
			if err := pub(content.KindItem, it.Ref); err != nil {
				return n, err
			}
		}
		for _, mb := range z.Mobs {
			if err := pub(content.KindMob, mb.Ref); err != nil {
				return n, err
			}
		}
		// The zone-SHAPE invalidation drives the live-room reconcile (#191): it carries the zone's full
		// authoritative room-ref set + start room, and the hosting shard converges its live room graph to
		// it (spawn ADDs, resync UPDATEs, tear down rooms the edit DELETED — the only signal a shard gets
		// that a live room is gone, since the per-ref loop above names only PRESENT refs). Published AFTER
		// this zone's per-ref invalidations so the prototype cache swaps land before the reconcile reads
		// them (serial ordered delivery). A zone is not a spawnable prototype, so it is NOT counted in n
		// (the published-definition tally the builder sees stays a prototype count).
		rooms := make([]string, 0, len(z.Rooms))
		for _, r := range z.Rooms {
			rooms = append(rooms, r.Ref)
		}
		if err := bus.Publish(ctx, Invalidation{
			Kind: content.KindZone, Ref: z.Ref, Pack: pk.Pack,
			Version: version, Rooms: rooms, StartRoom: z.StartRoom,
		}); err != nil {
			return n, err
		}
	}
	// Pack-GLOBAL channel_defs (Phase 8.3): a re-seed may have edited a channel's color/format/access,
	// so publish a `channel` invalidation per channel ref. The world's reloader swaps the channel
	// registry (world/reload.go reloadChannel). Channels are NOT under a zone, so this is the pack-level
	// loop, not the per-zone one. (Other pack globals — attributes/abilities/... — have no hot-reload
	// kind yet and are a boot concern; only channel is published here.)
	for _, ch := range pk.Channels {
		if err := pub(content.KindChannel, ch.Ref); err != nil {
			return n, err
		}
	}
	return n, nil
}
