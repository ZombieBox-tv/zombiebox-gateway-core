package media

import (
	"errors"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/subtitles"
)

const sidecarBase = 100000

type sidecar struct {
	path   string
	stream domain.Stream
}

// Only regular sibling files belonging to this exact media basename are candidates.
// Opaque stable IDs survive directory reordering; no supplied path is accepted.
func sidecars(path string) []sidecar {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil
	}
	defer directory.Close()
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var result []sidecar
	used := map[int]bool{}
	for scanned := 0; scanned < 4096 && len(result) < 16; scanned += 256 {
		entries, err := directory.ReadDir(256)
		for _, entry := range entries {
			name := entry.Name()
			extension := strings.ToLower(filepath.Ext(name))
			if extension != ".srt" && extension != ".vtt" && extension != ".ass" && extension != ".ssa" {
				continue
			}
			base := strings.TrimSuffix(name, filepath.Ext(name))
			if base != stem && !strings.HasPrefix(base, stem+".") {
				continue
			}
			info, statErr := entry.Info()
			if statErr != nil || !info.Mode().IsRegular() || entry.Type()&os.ModeSymlink != 0 || info.Size() > 2<<20 {
				continue
			}
			hash := fnv.New32a()
			_, _ = hash.Write([]byte(name))
			id := sidecarBase + int(hash.Sum32()&0x3fffffff)
			if used[id] {
				continue
			}
			stream := domain.Stream{Index: id, Type: "subtitle", Codec: strings.TrimPrefix(extension, ".")}
			suffix := strings.TrimPrefix(strings.TrimPrefix(base, stem), ".")
			for _, part := range strings.Split(suffix, ".") {
				switch strings.ToLower(part) {
				case "forced":
					stream.Disposition.Forced = 1
				case "default":
					stream.Disposition.Default = 1
				default:
					if stream.Tags.Language == "" && len(part) <= 15 {
						stream.Tags.Language = part
					}
				}
			}
			stream.Tags.Title = suffix
			used[id] = true
			result = append(result, sidecar{filepath.Join(filepath.Dir(path), name), stream})
			if len(result) == 16 {
				break
			}
		}
		if err != nil {
			break
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].path < result[j].path })
	return result
}

func readSidecar(path string) ([]domain.SubtitleCue, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("subtitle unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil {
		return nil, errors.New("subtitle unavailable")
	}
	return subtitles.ParseSubtitles(data)
}
