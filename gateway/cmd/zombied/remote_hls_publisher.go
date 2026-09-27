package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"zombiebox.local/gateway/internal/domain"
	mediatools "zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/server"
)

type remoteHLSPublisherAdapter struct {
	remote *mediatools.RemoteTools
}

func (a remoteHLSPublisherAdapter) StartRemoteHLSPublisher(ctx context.Context, source domain.Source, selection domain.MediaSelection, root string) (server.RemoteHLSPublication, error) {
	if a.remote == nil {
		return nil, errors.New("remote media unavailable")
	}
	session, err := a.remote.StartRemoteHLSPublisher(ctx, source, selection, root)
	if err != nil {
		return nil, err
	}
	return remoteHLSPublicationAdapter{session: session}, nil
}

type remoteHLSPublicationAdapter struct {
	session *mediatools.RemoteHLSPublisherSession
}

func (a remoteHLSPublicationAdapter) Snapshot() (server.RemoteHLSPublicationSnapshot, error) {
	if a.session == nil {
		return server.RemoteHLSPublicationSnapshot{}, errors.New("HLS publisher unavailable")
	}
	snapshot, err := a.session.Snapshot()
	if err != nil {
		return server.RemoteHLSPublicationSnapshot{}, err
	}
	result := server.RemoteHLSPublicationSnapshot{
		State:    string(snapshot.State),
		Playlist: append([]byte(nil), snapshot.Playlist...),
		Error:    snapshot.Error,
		Complete: snapshot.Complete,
		Segments: make([]server.RemoteHLSPublishedSegment, 0, len(snapshot.Segments)),
	}
	for _, segment := range snapshot.Segments {
		result.Segments = append(result.Segments, server.RemoteHLSPublishedSegment{Name: segment.Name, Bytes: segment.Bytes})
	}
	return result, nil
}

func (a remoteHLSPublicationAdapter) OpenSegment(name string) (server.ReadSeekCloser, int64, error) {
	if a.session == nil {
		return nil, 0, errors.New("HLS segment unavailable")
	}
	file, size, err := a.session.OpenSegment(name)
	if err != nil {
		return nil, 0, err
	}
	return file, size, nil
}

func (a remoteHLSPublicationAdapter) Close() {
	if a.session != nil {
		a.session.Close()
	}
}

func prepareYouTubeHLSPublishDir(raw string) error {
	if raw == "" {
		return errors.New("private YouTube HLS output directory required")
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return errors.New("invalid YouTube HLS output directory")
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return errors.New("unable to create YouTube HLS output directory")
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("YouTube HLS output directory must be a private directory")
	}
	if err := os.Chmod(absolute, 0700); err != nil {
		return errors.New("unable to secure YouTube HLS output directory")
	}
	info, err = os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0700 != 0700 {
		return errors.New("YouTube HLS output directory must be private and writable")
	}
	return nil
}

func resolvedYouTubeHLSPublishDir(statePath, configured string) string {
	if configured != "" {
		return configured
	}
	return filepath.Join(filepath.Dir(statePath), "youtube-hls")
}
