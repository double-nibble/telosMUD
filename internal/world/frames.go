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
