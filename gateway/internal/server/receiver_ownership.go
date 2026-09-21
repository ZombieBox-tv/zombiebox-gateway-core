package server

import "time"

// Claim transitions are serialized independently of network/session state locks.
// First explicitly armed receiver wins; users release it before choosing another.
func (s *Server) receiverBusy(device, requested string) bool {
	if requested != "media" && s.mediaReceiverInbox.Active(device) {
		return true
	}
	if requested != "youtube" && s.youtubeReceiver.Active(device) {
		return true
	}
	if requested != "cast" {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.casts {
			if c.receiver == device && time.Now().Before(c.expires) {
				return true
			}
		}
	}
	return false
}
