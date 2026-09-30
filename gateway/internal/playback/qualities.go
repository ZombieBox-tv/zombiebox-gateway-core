package playback

import (
	"strings"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
)

type QualityTier struct {
	ID      string
	Label   string
	Height  int
	Width   int
	Bitrate int64
}

var standardTiers = []QualityTier{
	{ID: "2160p", Label: "4K (2160p)", Height: 2160, Width: 3840, Bitrate: 12000000},
	{ID: "1440p", Label: "1440p", Height: 1440, Width: 2560, Bitrate: 8000000},
	{ID: "1080p", Label: "1080p", Height: 1080, Width: 1920, Bitrate: 4000000},
	{ID: "720p", Label: "720p", Height: 720, Width: 1280, Bitrate: 2000000},
	{ID: "480p", Label: "480p", Height: 480, Width: 854, Bitrate: 1200000},
	{ID: "360p", Label: "360p", Height: 360, Width: 640, Bitrate: 1000000},
	{ID: "240p", Label: "240p", Height: 240, Width: 426, Bitrate: 400000},
	{ID: "144p", Label: "144p", Height: 144, Width: 256, Bitrate: 200000},
}

// classifyDimensionsTier maps raw width/height to the highest standard 16:9 tier
// they fully reach, using integer/rational comparison (no premature rounding).
// Real height governs tall/portrait sources; real width recovers the nominal
// 16:9 height for panoramic/letterboxed sources (e.g. 1920x804 -> 1080p).
func classifyDimensionsTier(width, height int) string {
	if height <= 0 {
		return ""
	}
	heightX16 := height * 16
	widthX9 := width * 9
	effectiveX16 := heightX16
	if widthX9 > effectiveX16 {
		effectiveX16 = widthX9
	}
	switch {
	case effectiveX16 >= 1080*16:
		return "1080p"
	case effectiveX16 >= 720*16:
		return "720p"
	case effectiveX16 >= 480*16:
		return "480p"
	default:
		return "360p"
	}
}

// ClassifyDimensionsTier exposes the nominal standard tier implied by the real
// width/height so YouTube split-tier checks can compare against the same policy.
func ClassifyDimensionsTier(width, height int) string {
	return classifyDimensionsTier(width, height)
}

func findTier(id string) *QualityTier {
	for i := range standardTiers {
		if standardTiers[i].ID == id {
			return &standardTiers[i]
		}
	}
	return nil
}

func findVideoStream(streams []domain.Stream) *domain.Stream {
	var best *domain.Stream
	for i := range streams {
		if streams[i].Type == "video" {
			if best == nil || streams[i].Height > best.Height {
				best = &streams[i]
			}
		}
	}
	return best
}

func ValidQuality(quality string) bool {
	switch quality {
	case "", "auto", "STANDARD", "LOW", "144p", "240p", "360p", "480p", "720p", "1080p", "1440p", "2160p":
		return true
	default:
		return false
	}
}

func isProbePassing(caps domain.Capabilities, probeID string, now int64) bool {
	return probeStatus(caps, probeID, now) == "PASS"
}

func maxSupportedOutputHeight(device domain.Device) int {
	// Registration.Display and DisplayHint dimensions describe the Android UI
	// viewport. Overscan can make that viewport smaller than the video output
	// (the Vizio reports 1826x1026 while its 1080p decoder probe passes).
	// Only enumerated output modes are evidence for a hard output ceiling.
	if device.Registration.Hardware == nil {
		return 0
	}
	maxH := 0
	for _, disp := range device.Registration.Hardware.Displays {
		if !disp.Default {
			continue
		}
		for _, mode := range disp.Modes {
			if mode.Height > maxH {
				maxH = mode.Height
			}
		}
	}
	return maxH
}

func canTranscode(caps domain.Capabilities, now int64) bool {
	if !isProbePassing(caps, "http-fmp4", now) || !isProbePassing(caps, "aac", now) {
		return false
	}
	return isProbePassing(caps, "h264-baseline-360", now) ||
		isProbePassing(caps, "h264-baseline-480", now) ||
		isProbePassing(caps, "h264-720-main", now) ||
		isProbePassing(caps, "h264-720-high", now) ||
		isProbePassing(caps, "h264-1080-high", now) ||
		isProbePassing(caps, "h264-2160-high", now)
}

func canDecodeH264Tier(caps domain.Capabilities, tierID string, device domain.Device, now int64) bool {
	switch tierID {
	case "1080p":
		// Match the actual FFmpeg output codec (H.264) rather than accepting a HEVC-only decode probe as proof of H.264 transcode.
		return isProbePassing(caps, "h264-1080-high", now) || isProbePassing(caps, "h264-2160-high", now)
	case "720p":
		return isProbePassing(caps, "h264-720-main", now) ||
			isProbePassing(caps, "h264-720-high", now) ||
			isProbePassing(caps, "h264-1080-high", now) ||
			isProbePassing(caps, "h264-2160-high", now)
	case "480p":
		return isProbePassing(caps, "h264-baseline-480", now) ||
			isProbePassing(caps, "h264-720-main", now) ||
			isProbePassing(caps, "h264-720-high", now) ||
			isProbePassing(caps, "h264-1080-high", now) ||
			isProbePassing(caps, "h264-2160-high", now)
	case "360p", "240p", "144p":
		return isProbePassing(caps, "h264-baseline-360", now) ||
			isProbePassing(caps, "h264-baseline-480", now) ||
			isProbePassing(caps, "h264-720-main", now) ||
			isProbePassing(caps, "h264-720-high", now) ||
			isProbePassing(caps, "h264-1080-high", now) ||
			isProbePassing(caps, "h264-2160-high", now)
	default:
		return false
	}
}

func isNativeDirectPlayVerified(metadata domain.Metadata, source domain.Source, device domain.Device, caps domain.Capabilities, now int64) bool {
	if source.AudioURL != "" {
		return false
	}
	if needsCompatibleContainer(metadata.Format.Name, source.MIME) {
		return false
	}
	if source.MIME == "video/x-matroska" || source.MIME == "video/webm" {
		return false
	}
	for _, p := range caps.Probes {
		if p.ID == "http-progressive" && p.Status == "FAIL" {
			return false
		}
	}

	videoStream := findVideoStream(metadata.Streams)
	if videoStream == nil || videoStream.Height <= 0 {
		return false
	}

	audioFound := false
	hasAudioStream := false
	for _, stream := range metadata.Streams {
		if stream.Type == "audio" {
			hasAudioStream = true
			if stream.Codec == "aac" {
				if isProbePassing(caps, "aac", now) {
					audioFound = true
				}
			} else if stream.Codec == "mp3" {
				audioFound = true
			}
		}
	}
	if hasAudioStream && !audioFound {
		return false
	}

	maxOutputH := maxSupportedOutputHeight(device)
	if maxOutputH > 0 && maxOutputH < videoStream.Height {
		return false
	}

	if videoStream.Codec == "hevc" || videoStream.Width > 1920 || videoStream.Height > 1080 {
		return extendedVideoCandidate(*videoStream, metadata, caps)
	}

	if videoStream.Codec == "h264" {
		tierID := classifyDimensionsTier(videoStream.Width, videoStream.Height)
		return canDecodeH264Tier(caps, tierID, device, now)
	}

	return false
}

func currentCaps(device domain.Device) domain.Capabilities {
	c := device.Capabilities
	if c.SuiteVersion != 0 && c.SuiteVersion != devices.ProbeSuiteVersion {
		return domain.Capabilities{Version: 1, DeviceID: device.ID, Probes: []domain.Probe{}}
	}
	return c
}

func hasVariant(variants []string, id string) bool {
	for _, v := range variants {
		if v == id {
			return true
		}
	}
	return false
}

// Qualities computes the selectable playback renditions bounded by source resolution
// and validated device capability probes. Providers without multiple known renditions
// offer Auto only. Selectable options require positive PASS evidence for output format,
// transport, audio, and target tier decode. Missing, unknown, stale or failed probes
// cannot manufacture support.
func Qualities(metadata domain.Metadata, source domain.Source, device domain.Device, currentQuality string) domain.QualityInventory {
	autoInventory := domain.QualityInventory{
		SelectedID: "auto",
		Options: []domain.QualityOption{
			{ID: "auto", Label: "Auto"},
		},
	}

	// Providers/sources without multiple known renditions offer Auto only.
	if source.Live || source.Item.Kind == "track" || source.Item.Provider == "iptv" {
		return autoInventory
	}

	videoStream := findVideoStream(metadata.Streams)
	if (videoStream == nil || videoStream.Height <= 0) && len(source.Variants) == 0 {
		return autoInventory
	}
	srcHeight := 0
	if videoStream != nil {
		srcHeight = videoStream.Height
	}

	caps := currentCaps(device)
	now := time.Now().Unix()
	transcodeOK := canTranscode(caps, now)
	nativeVerified := isNativeDirectPlayVerified(metadata, source, device, caps, now)
	maxOutputH := maxSupportedOutputHeight(device)

	options := []domain.QualityOption{
		{ID: "auto", Label: "Auto"},
	}

	for _, tier := range standardTiers {
		if len(source.Variants) > 0 {
			// For sources with validated upstream variants (like YouTube),
			// only consider tiers actually present in upstream variants:
			if !hasVariant(source.Variants, tier.ID) {
				continue
			}
		} else {
			// Source bounding: never fabricate entries higher than the source rendition.
			if tier.Height > srcHeight {
				continue
			}
		}

		// UHD needs positive output-mode evidence. A logical UI viewport is
		// never proof that 1080p video is impossible.
		if tier.Height > 1080 && maxOutputH == 0 {
			continue
		}
		// Output evidence bounding: avoid impossible options if display output is known and too small.
		if maxOutputH > 0 && maxOutputH < tier.Height {
			continue
		}
		if len(source.Variants) > 0 {
			// The wrapper validates each upstream rendition. A variant can be
			// progressive H.264/AAC or separate tracks; the current Auto stream
			// does not describe its container. Admit the candidate if either
			// transport passed and verify the resolved source before switching.
			if !canDecodeH264Tier(caps, tier.ID, device, now) || !isProbePassing(caps, "aac", now) ||
				(!isProbePassing(caps, "http-progressive", now) && !isProbePassing(caps, "http-fmp4", now)) {
				continue
			}
			options = append(options, domain.QualityOption{ID: tier.ID, Label: tier.Label, Width: tier.Width, Height: tier.Height})
			continue
		}

		if tier.Height == srcHeight {
			// Native resolution:
			// Direct play is selectable if genuinely verified, without requiring transcode evidence.
			if nativeVerified {
				options = append(options, domain.QualityOption{
					ID:     tier.ID,
					Label:  tier.Label,
					Width:  tier.Width,
					Height: tier.Height,
				})
				continue
			}
			// If not verified for direct play, can only be offered if transcoding is supported and capable.
			if !transcodeOK || tier.Height > 1080 || !canDecodeH264Tier(caps, tier.ID, device, now) {
				continue
			}
			options = append(options, domain.QualityOption{
				ID:     tier.ID,
				Label:  tier.Label,
				Width:  tier.Width,
				Height: tier.Height,
			})
			continue
		}

		// Alternative or downscaled tiers:
		// These require H.264 decode capability, valid fmp4+aac transcode evidence, and <= 1080p.
		if tier.Height > 1080 {
			continue
		}
		if !transcodeOK {
			continue
		}
		if !canDecodeH264Tier(caps, tier.ID, device, now) {
			continue
		}

		options = append(options, domain.QualityOption{
			ID:     tier.ID,
			Label:  tier.Label,
			Width:  tier.Width,
			Height: tier.Height,
		})
	}

	// For content with no selectable alternative renditions, offer Auto only.
	if len(options) <= 1 {
		return autoInventory
	}

	selectedID := "auto"
	norm := currentQuality
	if norm == "STANDARD" {
		norm = "360p"
	} else if norm == "LOW" {
		norm = "240p"
	}
	if norm != "" && norm != "auto" {
		for _, opt := range options {
			if opt.ID == norm {
				selectedID = norm
				break
			}
		}
	}

	return domain.QualityInventory{
		SelectedID: selectedID,
		Options:    options,
	}
}

func HasQuality(inventory domain.QualityInventory, qualityID string) bool {
	if qualityID == "auto" {
		return true
	}
	if qualityID == "STANDARD" {
		qualityID = "360p"
	} else if qualityID == "LOW" {
		qualityID = "240p"
	}
	for _, opt := range inventory.Options {
		if opt.ID == qualityID {
			return true
		}
	}
	return false
}

func youtubeKnownLengthRemuxEligible(device domain.Device, source domain.Source, now time.Time) bool {
	if source.Item.Provider != "youtube" || source.Item.Kind != "video" || source.Live || source.Path != "" || source.URL == "" || source.AudioURL == "" {
		return false
	}
	caps := currentCaps(device)
	if caps.SuiteVersion != devices.ProbeSuiteVersion || caps.DeviceID != device.ID || caps.CacheKey == "" || caps.CacheKey != devices.ProbeCacheKey(device) {
		return false
	}
	if !isProbePassing(caps, "http-fmp4", now.Unix()) || !isProbePassing(caps, "aac", now.Unix()) {
		return false
	}
	if !isProbePassing(caps, "h264-1080-high", now.Unix()) && !isProbePassing(caps, "h264-2160-high", now.Unix()) {
		return false
	}
	for index := range caps.Probes {
		probe := &caps.Probes[index]
		if probe.ID != "http-fmp4-chunked" {
			continue
		}
		if probe.TestedAt <= 0 || probe.TestedAt <= now.Unix()-7*24*60*60 || probe.TestedAt > now.Unix()+300 {
			continue
		}
		if probe.Status == "PASS" || probe.Stalled {
			return false
		}
		if probe.Status == "FAIL" {
			return true
		}
		if probe.Status == "UNKNOWN" && strings.Contains(strings.ToLower(probe.Detail), "@prepare") && strings.Contains(strings.ToLower(probe.Detail), "what=0") {
			return true
		}
	}
	return false
}

func RequiresTranscodeForQuality(metadata domain.Metadata, qualityID string) bool {
	if qualityID == "" || qualityID == "auto" {
		return false
	}
	if qualityID == "STANDARD" {
		qualityID = "360p"
	} else if qualityID == "LOW" {
		qualityID = "240p"
	}
	tier := findTier(qualityID)
	if tier == nil {
		return false
	}
	videoStream := findVideoStream(metadata.Streams)
	if videoStream == nil || videoStream.Height <= 0 {
		return false
	}
	return tier.Height < videoStream.Height
}

// SelectedQualityMode determines the playback mode and quality parameter when a user
// selects a rendition or reverts to Auto. It respects the DIRECT_PLAY -> REMUX -> TRANSCODE policy.
func SelectedQualityMode(metadata domain.Metadata, source domain.Source, device domain.Device, qualityID string, positionMS int64, currentMode string) (string, string) {
	caps := currentCaps(device)
	if source.AudioURL != "" && !isProbePassing(caps, "http-fmp4", time.Now().Unix()) {
		return "EXTERNAL_PLAYER", ""
	}
	if qualityID == "" || qualityID == "auto" {
		if source.AudioURL != "" {
			// Separate audio and video streams (e.g. YouTube adaptive):
			// DIRECT_PLAY is never supported for separate tracks on the client.
			if positionMS > 0 {
				return "TRANSCODE", ""
			}
			tierID := ""
			videoStream := findVideoStream(metadata.Streams)
			if videoStream != nil {
				tierID = classifyDimensionsTier(videoStream.Width, videoStream.Height)
			}
			if tierID != "" && canDecodeH264Tier(caps, tierID, device, time.Now().Unix()) && isProbePassing(caps, "http-fmp4", time.Now().Unix()) {
				return "REMUX", ""
			}
			return "TRANSCODE", ""
		}

		mode := LocalModeSource(metadata, source, caps, "")
		if positionMS > 0 && (mode == "REMUX" || mode == "HYBRID") {
			mode = "TRANSCODE"
		}
		return mode, ""
	}

	norm := qualityID
	if norm == "STANDARD" {
		norm = "360p"
	} else if norm == "LOW" {
		norm = "240p"
	}

	videoStream := findVideoStream(metadata.Streams)
	srcHeight := 0
	if videoStream != nil {
		srcHeight = videoStream.Height
	}
	tier := findTier(norm)

	// If chosen quality matches native resolution exactly and source can direct play:
	if tier != nil && srcHeight > 0 && tier.Height == srcHeight && source.AudioURL == "" {
		mode := LocalModeSource(metadata, source, caps, "")
		if mode == "DIRECT_PLAY" {
			return "DIRECT_PLAY", norm
		}
	}

	// For separate audio and video streams (e.g. YouTube adaptive):
	// DIRECT_PLAY -> REMUX -> TRANSCODE. Preserve the legacy behavior for
	// ordinary split-tier requests, but allow the known-length REMUX fallback to
	// be evaluated before a nonzero position forces TRANSCODE. This keeps the
	// default 720p semantics unchanged while still enabling the narrow 804p/known-
	// length recovery path.
	if source.AudioURL != "" {
		if tier != nil && canDecodeH264Tier(caps, tier.ID, device, time.Now().Unix()) && isProbePassing(caps, "http-fmp4", time.Now().Unix()) {
			if positionMS == 0 || (source.Item.Provider == "youtube" && youtubeKnownLengthRemuxEligible(device, source, time.Now())) {
				return "REMUX", norm
			}
		}
		if positionMS > 0 {
			return "TRANSCODE", norm
		}
	}

	return "TRANSCODE", norm
}
