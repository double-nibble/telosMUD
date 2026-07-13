package commbus

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// jetstream_parkadvisory_test.go holds the HERMETIC guards for the MAX_DELIVERIES park advisory monitor
// (#311): the advisory subject + queue-group construction, and the parse/count callback. The end-to-end
// delivery over a real broker is the gated TestJetStreamRealParkAdvisory (jetstream_nats_test.go) — an
// advisory is a broker-published signal the MemJetStream stand-in cannot produce, so the wiring is pinned
// hermetically and the behavior is pinned gated (the same split the #266 MaxAckPending guard uses).

func TestParkAdvisorySubjectAndQueue(t *testing.T) {
	// Filtered to THIS stream's consumers, trailing ">" for the per-consumer token. The prefix is the broker's
	// documented advisory subject — a change here would silently stop counting every park.
	assert.Equal(t, "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.COMMS_TELL.>", parkAdvisorySubject("COMMS_TELL"))
	assert.Equal(t, "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.WORLD_EVENTS.>", parkAdvisorySubject("WORLD_EVENTS"))

	// The queue-group name is per-stream and STABLE: every process on the stream must join the SAME group so
	// the broker delivers each advisory to exactly one member (counting a park once cluster-wide, not once per
	// process). A per-process-unique name here would reintroduce the N-fold overcount the queue group prevents.
	assert.Equal(t, "telos-parkmon-COMMS_TELL", parkAdvisoryQueue("COMMS_TELL"))
	assert.Equal(t, "telos-parkmon-WORLD_EVENTS", parkAdvisoryQueue("WORLD_EVENTS"))
}

func TestHandleParkAdvisoryCountsAndObserves(t *testing.T) {
	b := &NATSJetStream{name: "COMMS_TELL", log: slog.Default()}

	var gotStream, gotConsumer string
	var gotSeq uint64
	calls := 0
	obs := parkObserver(func(stream, consumer string, seq uint64) {
		calls++
		gotStream, gotConsumer, gotSeq = stream, consumer, seq
	})
	parkAdvisoryObserver.Store(&obs)
	t.Cleanup(func() { parkAdvisoryObserver.Store(nil) })

	// A well-formed advisory: the callback fires once with the parsed consumer + seq (the counter is labeled
	// by stream regardless).
	payload, err := json.Marshal(maxDeliveriesAdvisory{
		Stream: "COMMS_TELL", Consumer: "alice", StreamSeq: 42, Deliveries: 10,
	})
	assert.NoError(t, err)
	b.handleParkAdvisory(payload)
	assert.Equal(t, 1, calls, "a valid advisory must count exactly one park")
	assert.Equal(t, "COMMS_TELL", gotStream)
	assert.Equal(t, "alice", gotConsumer)
	assert.Equal(t, uint64(42), gotSeq)

	// A MALFORMED payload still counts the park (the advisory subject alone proves a park happened) — it must
	// never silently swallow a permanent-loss event just because the body didn't parse.
	b.handleParkAdvisory([]byte("{not json"))
	assert.Equal(t, 2, calls, "a malformed advisory must still count the park")
	assert.Equal(t, "COMMS_TELL", gotStream)
	assert.Equal(t, "", gotConsumer, "an unparseable payload yields no consumer detail")
	assert.Equal(t, uint64(0), gotSeq)
}
