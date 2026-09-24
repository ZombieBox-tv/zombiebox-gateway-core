package media

// Fixed, bounded encoder profiles. Client input can select a profile, never flags.
func ValidQuality(quality string) bool {
	switch quality {
	case "", "auto", "STANDARD", "LOW", "144p", "240p", "360p", "480p", "720p", "1080p":
		return true
	default:
		return false
	}
}

func videoEncoding(quality string) []string {
	scale, bitrate, maximum, buffer, audio := "640:360", "1000k", "1200k", "2400k", "128k"
	profile, level := "baseline", "3.0"
	switch quality {
	case "1080p":
		scale, bitrate, maximum, buffer, audio = "1920:1080", "4000k", "5000k", "8000k", "192k"
		profile, level = "high", "4.0"
	case "720p":
		scale, bitrate, maximum, buffer, audio = "1280:720", "2000k", "2500k", "4000k", "128k"
		profile, level = "main", "3.1"
	case "480p":
		scale, bitrate, maximum, buffer, audio = "854:480", "1200k", "1500k", "2400k", "128k"
		profile, level = "baseline", "3.0"
	case "360p", "STANDARD", "":
		scale, bitrate, maximum, buffer, audio = "640:360", "1000k", "1200k", "2400k", "128k"
		profile, level = "baseline", "3.0"
	case "240p", "LOW":
		scale, bitrate, maximum, buffer, audio = "426:240", "400k", "500k", "1000k", "64k"
		profile, level = "baseline", "3.0"
	case "144p":
		scale, bitrate, maximum, buffer, audio = "256:144", "200k", "250k", "500k", "64k"
		profile, level = "baseline", "3.0"
	}
	return []string{
		"-c:v", "libx264", "-threads", "2", "-filter_threads", "1",
		"-preset", "veryfast", "-profile:v", profile, "-level:v", level,
		"-pix_fmt", "yuv420p", "-vf", "scale=" + scale + ":force_original_aspect_ratio=decrease:force_divisible_by=2",
		"-r", "30", "-b:v", bitrate, "-maxrate", maximum, "-bufsize", buffer,
		"-c:a", "aac", "-b:a", audio, "-ac", "2", "-ar", "44100",
	}
}
