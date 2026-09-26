package domain

// ArtworkProfile selects a bounded derivative, never an arbitrary allocation size.
type ArtworkProfile string

const (
	ArtworkLandscapeSmall  ArtworkProfile = "landscape-small"
	ArtworkLandscapeMedium ArtworkProfile = "landscape-medium"
	ArtworkPosterSmall     ArtworkProfile = "poster-small"
	ArtworkPosterMedium    ArtworkProfile = "poster-medium"
	ArtworkAudioSmall      ArtworkProfile = "audio-small"
	ArtworkAudioMedium     ArtworkProfile = "audio-medium"
	ArtworkHeroSmall       ArtworkProfile = "hero-small"
	ArtworkHeroMedium      ArtworkProfile = "hero-medium"
)
