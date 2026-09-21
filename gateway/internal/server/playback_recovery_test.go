package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

func TestExplicitResumePositionOverridesHistoryIncludingZero(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.mp4"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, nil, dir)
	token := pair(t, s, "recovery-device")
	sources, err := providers.Local(dir)
	if err != nil || len(sources) != 1 {
		t.Fatal(err, sources)
	}
	item := sources[0].Item.ID
	if err := s.db.Put(t.Context(), "progress:recovery-device", item, domain.Progress{PositionMS: 10000, State: "ENDED"}); err != nil {
		t.Fatal(err)
	}
	for _, position := range []int{0, 42000} {
		w := call(s, "POST", "/v1/playback", fmt.Sprintf(`{"itemId":%q,"mode":"DIRECT_PLAY","positionMs":%d}`, item, position), "recovery-device", token, "")
		var plan domain.Plan
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &plan) != nil {
			t.Fatal(w.Code, w.Body)
		}
		if plan.ResumeMS != int64(position) {
			t.Fatal("lost explicit retry position", plan)
		}
		call(s, "DELETE", "/v1/playback/"+plan.SessionID, "", "recovery-device", token, "")
	}
	for _, position := range []string{"-1", "604800001", "1.5", `"42"`} {
		w := call(s, "POST", "/v1/playback", fmt.Sprintf(`{"itemId":%q,"positionMs":%s}`, item, position), "recovery-device", token, "")
		if w.Code != 400 {
			t.Fatal("accepted invalid position", position, w.Code)
		}
	}
}
