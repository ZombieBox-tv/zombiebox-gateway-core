package media

import (
	"context"
	"io"
	"os/exec"
	"testing"
)

func TestPipelineDiagnosticEncodesConvertsAndDecodes(t *testing.T) {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " absent")
		}
	}
	report := New("ffmpeg", "ffprobe").DiagnosePipeline(t.Context())
	if !report.Verified || report.Fixture.State != "pass" || report.ReceiverValidated || report.NetworkValidated {
		t.Fatalf("unexpected evidence: %+v", report)
	}
	if report.Transcode.AudioFrames < 5 || report.Remux.VideoFrames < 5 {
		t.Fatal("no decoded media", report)
	}
}

type cancelledDiagnosticRunner struct{}

func (cancelledDiagnosticRunner) Run(ctx context.Context, _ string, _ []string, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestCancelledDiagnosticDoesNotClaimCodecFailureOrRunFollowingStages(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report := NewWithRunner("ffmpeg", "ffprobe", cancelledDiagnosticRunner{}).DiagnosePipeline(ctx)
	if report.Verified || report.Fixture.State != "cancelled" || report.Remux.State != "not_run" || report.Transcode.State != "not_run" {
		t.Fatal(report)
	}
}

func TestDecoderEvidenceRequiresBothStreamsAndRejectsMalformedOutput(t *testing.T) {
	for _, text := range []string{"", "# successful metadata only\n", "0,0,0,1,100,hash\n", "0,0,0,1,not-size,hash\n", "2,0,0,1,100,hash\n"} {
		if _, _, err := decodedFrames(text); err == nil {
			t.Fatal("accepted incomplete decoding", text)
		}
	}
}

func TestMissingDiagnosticToolsRemainUnavailable(t *testing.T) {
	report := New("/zombie-test-missing-ffmpeg", "/zombie-test-missing-ffprobe").DiagnosePipeline(t.Context())
	if report.Verified || report.Fixture.State != "unavailable" || report.Remux.State != "not_run" {
		t.Fatal(report)
	}
}
