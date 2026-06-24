package world

import playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"

// Frame constructors for the world->gate direction. Phase 1 emits plain markup;
// the gate renders it (docs/PROTOCOL.md D1).

func textFrame(markup string) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Output{Output: &playv1.Output{
		Markup: markup,
		Class:  playv1.OutputClass_OUTPUT_CLASS_NORMAL,
	}}}
}

func promptFrame() *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Prompt{Prompt: &playv1.PromptUpdate{
		Markup: "> ",
	}}}
}

func attachedFrame(shardID string) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Attached{Attached: &playv1.Attached{
		ShardId: shardID,
	}}}
}

func disconnectFrame(reason string) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Disconnect{Disconnect: &playv1.Disconnect{
		Reason: reason,
	}}}
}
