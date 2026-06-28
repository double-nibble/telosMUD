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

// PublishPack publishes an invalidation for every room, item, and mob prototype in pk on bus. It
// is the seed/OLC trigger: a running shard subscribed to the bus re-reads and swaps each ref. The
// zone definition itself is not a spawnable prototype, so it is not published (a zone-shape change
// is a boot concern). Errors from individual publishes are returned at the first failure so the
// caller can log; a partial publish still hot-reloads the refs that made it onto the wire.
func PublishPack(ctx context.Context, bus Bus, pk content.Pack) (int, error) {
	if bus == nil {
		return 0, nil
	}
	n := 0
	pub := func(kind, ref string) error {
		if err := bus.Publish(ctx, Invalidation{Kind: kind, Ref: ref, Pack: pk.Pack}); err != nil {
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
