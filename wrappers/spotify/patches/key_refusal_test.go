//go:build test_unit

package daemon

import (
	"errors"
	"fmt"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/audio"
	"github.com/devgianlu/go-librespot/player"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/stretchr/testify/require"
)

func newKeyRefusalTestPlayer(t *testing.T) *AppPlayer {
	t.Helper()

	log := &librespot.NullLogger{}
	server, err := NewStubApiServer(log)
	require.NoError(t, err)

	pl, err := player.NewPlayer(&player.Options{
		Log:          log,
		AudioBackend: "pipe",
	})
	require.NoError(t, err)
	t.Cleanup(func() { pl.Close() })

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	p := &AppPlayer{
		app:        &App{log: log, server: server, cfg: &Config{DisableAutoplay: true}},
		state:      &State{},
		player:     pl,
		statePush:  newTestStatePushLane(nil),
		stateTimer: timer,
		loader:     newTestLoaderLane(),
	}
	p.state.reset()
	return p
}

func setKeyRefusalTestTrack(p *AppPlayer) string {
	const uri = "spotify:track:2FY7b99s15jUprqC0M5NCT"
	p.state.player.ContextUri = "spotify:playlist:context"
	p.state.player.Track = &connectpb.ProvidedTrack{Uri: uri}
	p.state.player.IsPlaying = true
	p.state.player.IsBuffering = true
	p.state.player.PlaybackSpeed = 0
	return uri
}

func TestKeyRefusalDoesNotAdvanceInitialLoadOrReportPlayback(t *testing.T) {
	p := newKeyRefusalTestPlayer(t)
	uri := setKeyRefusalTestTrack(p)
	keyErr := &audio.KeyProviderError{Code: 1}

	require.True(t, p.stopAfterKeyRefusal(keyErr))

	var callbackErr error
	p.handleCurrentTrackLoadError(uri, false, keyErr, func(err error) {
		callbackErr = err
	})

	require.ErrorIs(t, callbackErr, keyErr)
	require.Equal(t, uri, p.state.player.Track.GetUri())
	require.Equal(t, "spotify:playlist:context", p.state.player.ContextUri)
	require.Empty(t, p.loader.queue, "key refusal must not queue a next-track load")
	require.False(t, p.state.player.IsPlaying)
	require.False(t, p.state.player.IsPaused)
	require.False(t, p.state.player.IsBuffering)
	require.Zero(t, p.state.player.PlaybackSpeed)
}

func TestKeyRefusalDoesNotAdvanceSequentialTrack(t *testing.T) {
	p := newKeyRefusalTestPlayer(t)
	uri := setKeyRefusalTestTrack(p)
	p.consecutiveUnplayableSkips = 4
	keyErr := &audio.KeyProviderError{Code: 1}

	require.True(t, p.stopAfterKeyRefusal(keyErr))
	called := false
	var hasNext bool
	var callbackErr error
	p.handleAdvanceToLoadError(uri, false, keyErr, func(next bool, err error) {
		called = true
		hasNext = next
		callbackErr = err
	})

	require.True(t, called)
	require.False(t, hasNext)
	require.ErrorIs(t, callbackErr, keyErr)
	require.Equal(t, 0, p.consecutiveUnplayableSkips)
	require.Empty(t, p.loader.queue, "key refusal must not schedule another track")
	require.Equal(t, uri, p.state.player.Track.GetUri())
	require.Equal(t, "spotify:playlist:context", p.state.player.ContextUri)
}

func TestRestrictedAndUnsupportedMediaStillScheduleRecovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "restricted", err: librespot.ErrMediaRestricted},
		{name: "unsupported", err: librespot.ErrNoSupportedFormats},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newKeyRefusalTestPlayer(t)
			uri := setKeyRefusalTestTrack(p)
			p.handleCurrentTrackLoadError(uri, false, tc.err, func(error) {})

			require.Len(t, p.loader.queue, 1, "recoverable media errors should schedule the next load")
			require.Equal(t, "load "+uri, p.loader.queue[0].name)
		})
	}
}

func TestRestrictedAndUnsupportedMediaStopAtSkipBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "restricted", err: librespot.ErrMediaRestricted},
		{name: "unsupported", err: librespot.ErrNoSupportedFormats},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newKeyRefusalTestPlayer(t)
			uri := setKeyRefusalTestTrack(p)
			p.consecutiveUnplayableSkips = maxConsecutiveUnplayableSkips

			called := false
			var callbackErr error
			p.handleAdvanceToLoadError(uri, false, tc.err, func(_ bool, err error) {
				called = true
				callbackErr = err
			})

			require.True(t, called)
			require.ErrorIs(t, callbackErr, tc.err)
			require.Zero(t, p.consecutiveUnplayableSkips)
			require.Empty(t, p.loader.queue, "the skip bound must stop recovery")
			require.False(t, p.state.player.IsPlaying)
			require.False(t, p.state.player.IsPaused)
			require.False(t, p.state.player.IsBuffering)
			require.Zero(t, p.state.player.PlaybackSpeed)
		})
	}
}

func TestUnplayableAndSkippableMediaClassifyKeyRefusalSeparately(t *testing.T) {
	keyErr := fmt.Errorf("track load failed: %w", &audio.KeyProviderError{Code: 1})
	require.True(t, isUnplayableMedia(keyErr))
	require.False(t, isSkippableMedia(keyErr))
	require.True(t, isUnplayableMedia(librespot.ErrMediaRestricted))
	require.True(t, isSkippableMedia(librespot.ErrMediaRestricted))
	require.True(t, isUnplayableMedia(librespot.ErrNoSupportedFormats))
	require.True(t, isSkippableMedia(librespot.ErrNoSupportedFormats))
	require.False(t, isUnplayableMedia(errors.New("network timeout")))
	require.False(t, isSkippableMedia(errors.New("network timeout")))
}
