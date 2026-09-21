package media

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSidecarOwnershipStableIdentityAndCueBounds(t *testing.T) {
	directory := t.TempDir()
	media := filepath.Join(directory, "movie.mkv")
	_ = os.WriteFile(media, []byte("fixture"), 0600)
	caption := []byte("1\n00:00:01,000 --> 00:00:02,000\nHola\n")
	_ = os.WriteFile(filepath.Join(directory, "movie.es.forced.srt"), caption, 0600)
	_ = os.WriteFile(filepath.Join(directory, "another.es.srt"), caption, 0600)
	_ = os.Symlink("/etc/passwd", filepath.Join(directory, "movie.en.srt"))
	candidates := sidecars(media)
	if len(candidates) != 1 || candidates[0].stream.Tags.Language != "es" || candidates[0].stream.Disposition.Forced != 1 {
		t.Fatal(candidates)
	}
	id := candidates[0].stream.Index
	tools := New("unused", "unused")
	cues, err := tools.Subtitles(t.Context(), media, id)
	if err != nil || len(cues) != 1 || cues[0].Text != "Hola" {
		t.Fatal(cues, err)
	}
	_ = os.WriteFile(filepath.Join(directory, "movie.de.srt"), caption, 0600)
	cues, err = tools.Subtitles(t.Context(), media, id)
	if err != nil || len(cues) != 1 || cues[0].Text != "Hola" {
		t.Fatal("directory order changed identity", err)
	}
	_ = os.Remove(filepath.Join(directory, "movie.es.forced.srt"))
	if _, err := tools.Subtitles(t.Context(), media, id); err == nil {
		t.Fatal("removed sidecar resolved another track")
	}
}
