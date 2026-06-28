package world

import playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"

// Frame constructors for the world->gate direction. Each returns a *ServerFrame
// ready to hand to player.send. Phase 1 emits plain markup; the gate renders it
// (docs/PROTOCOL.md D1). Keeping construction here means the zone logic deals in
// intent ("show this text", "prompt", "disconnect") rather than protobuf shapes.

// textFrame wraps room/command output as a normal Output frame.
func textFrame(markup string) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Output{Output: &playv1.Output{
		Markup: markup,
		Class:  playv1.OutputClass_OUTPUT_CLASS_NORMAL,
	}}}
}

// promptFrame is the "> " prompt sent after each handled line of input.
func promptFrame() *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Prompt{Prompt: &playv1.PromptUpdate{
		Markup: "> ",
	}}}
}

// attachedFrame acknowledges a successful Attach, naming the shard the player
// landed on. Sent before the join is posted to the zone.
func attachedFrame(shardID string) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Attached{Attached: &playv1.Attached{
		ShardId: shardID,
	}}}
}

// disconnectFrame tells the gate to close the player's stream (e.g. after "quit").
func disconnectFrame(reason string) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Disconnect{Disconnect: &playv1.Disconnect{
		Reason: reason,
	}}}
}

// displacedNotice is the player-visible line a connection gets when a SECOND login for the
// same character takes over the session (single-session contract): the old connection is
// cleanly kicked rather than left mute. Mirrors the "Farewell." farewell style of quit.
const displacedNotice = "You have been disconnected: your character logged in from another location."

// displacedKick delivers the single-session takeover farewell to a DISPLACED connection's
// out channel and tells its gate to close the socket — the same notice + Disconnect shape
// "quit" emits, but aimed at the OLD channel (the session's out is being reassigned to the
// new connection, so we cannot route through session.send). It is non-blocking like
// session.send: if the displaced writer goroutine has already stopped (a near-simultaneous
// real drop) the frames are dropped and the socket is gone anyway. ack stamps the frames'
// ack_input_seq so the gate's buffer accounting stays consistent on the way out.
func displacedKick(out chan *playv1.ServerFrame, ack uint64) {
	notice := textFrame(displacedNotice)
	notice.AckInputSeq = ack
	bye := disconnectFrame("logged in elsewhere")
	bye.AckInputSeq = ack
	for _, f := range []*playv1.ServerFrame{notice, bye} {
		select {
		case out <- f:
		default:
		}
	}
}

// redirectFrame tells the gate to re-dial another shard (a cross-shard handoff,
// docs/PROTOCOL.md §5): the target address, the handoff token to present, and the
// input seq to replay from.
func redirectFrame(targetAddr, token string, resumeSeq uint64) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Redirect{Redirect: &playv1.Redirect{
		TargetShardAddr: targetAddr,
		HandoffToken:    token,
		ResumeInputSeq:  resumeSeq,
	}}}
}
