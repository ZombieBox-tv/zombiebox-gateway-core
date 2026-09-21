package youtube

import (
	"regexp"

	"zombiebox.local/gateway/internal/domain"
)

var receiverIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var receiverVideoPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var tvCodePattern = regexp.MustCompile(`^[0-9][0-9 -]{4,30}$`)

func ValidState(value domain.YouTubeReceiverState, id string) bool {
	if value.ReceiverID != id || (value.TVCode != "" && !tvCodePattern.MatchString(value.TVCode)) {
		return false
	}
	switch value.State {
	case "STARTING", "WAITING", "READY", "UNAVAILABLE":
	default:
		return false
	}
	if c := value.Command; c != nil {
		if !receiverIDPattern.MatchString(c.ID) || c.PositionMS < 0 || c.PositionMS > 604800000 || c.Volume < 0 || c.Volume > 100 {
			return false
		}
		switch c.Action {
		case "play":
			return receiverVideoPattern.MatchString(c.VideoID)
		case "pause", "resume", "stop", "seek", "volume":
		default:
			return false
		}
	}
	return true
}

func ValidAcknowledgement(state domain.ReceiverAcknowledgement) bool {
	if (state.CommandID != "" && !receiverIDPattern.MatchString(state.CommandID)) || state.PositionMS < 0 || state.DurationMS < 0 || state.PositionMS > 604800000 || state.DurationMS > 604800000 || state.Volume < 0 || state.Volume > 100 {
		return false
	}
	switch state.State {
	case "PLAYING", "PAUSED", "STOPPED", "ENDED", "BUFFERING", "FAILED":
		return true
	}
	return false
}
