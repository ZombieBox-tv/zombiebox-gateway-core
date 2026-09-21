package media

// Fixed, bounded encoder profiles. Client input can select a profile, never flags.
func videoEncoding(quality string) []string {
	scale, bitrate, maximum, buffer, audio := "640:360", "1000k", "1200k", "2400k", "128k"
	if quality == "LOW" {
		scale, bitrate, maximum, buffer, audio = "426:240", "400k", "500k", "1000k", "64k"
	}
	return []string{
		"-c:v", "libx264", "-threads", "2", "-filter_threads", "1",
		"-preset", "veryfast", "-profile:v", "baseline", "-level:v", "3.0",
		"-pix_fmt", "yuv420p", "-vf", "scale=" + scale + ":force_original_aspect_ratio=decrease:force_divisible_by=2",
		"-r", "30", "-b:v", bitrate, "-maxrate", maximum, "-bufsize", buffer,
		"-c:a", "aac", "-b:a", audio, "-ac", "2", "-ar", "44100",
	}
}
