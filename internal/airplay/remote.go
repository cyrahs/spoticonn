package airplay

import (
	"context"
	"strings"
	"time"
)

// RemoteCommand is a receiver request, separate from sender status/metadata.
// Context ends with the physical sender (including a cancelled group join).
// Consumers must check it again when dequeuing a request.
type RemoteCommand struct {
	Context    context.Context
	DeviceID   string
	Action     string
	ReceivedAt time.Time
}

// cliairplay v0.5.3 emits this exact stdout contract from its MediaRemote
// callback. Do not interpret debug messages, metadata or STATUS as commands.
func remoteAction(line string) string {
	action, ok := strings.CutPrefix(line, "[EVENT] remote command=")
	if !ok {
		return ""
	}
	switch action {
	case "play", "pause", "play_pause", "next", "previous":
		return action
	default:
		return ""
	}
}
