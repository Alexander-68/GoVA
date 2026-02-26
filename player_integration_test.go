package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSeekAfterPauseRendersRequestedFrame(t *testing.T) {
	skipIfNoFFmpeg(t)

	p := NewPlayer(600)
	defer p.stopDecoder()

	videoPath := createIntegrationVideo(t)
	if err := p.Load(videoPath); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.TotalFrames > 0 && s.CurrentFrame == 0 && !s.Playing
	})

	expectedFrame1 := seekAndCaptureFrame(t, p, 1)

	p.Seek(50)
	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.CurrentFrame == 50 && !s.Playing
	})

	p.Play()
	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.Playing && s.CurrentFrame >= 80
	})

	p.Pause()
	waitForState(t, p, 3*time.Second, func(s State) bool {
		return !s.Playing
	})

	p.Seek(1)
	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.CurrentFrame == 1 && !s.Playing
	})

	got := cloneBytes(p.GetCurrentImage())
	if !bytes.Equal(got, expectedFrame1) {
		t.Fatalf(
			"seek after pause rendered unexpected frame: got=%s expected=%s",
			shortHash(got),
			shortHash(expectedFrame1),
		)
	}
}

func TestPauseSeekFlushesBufferedStreamToLatestFrame(t *testing.T) {
	skipIfNoFFmpeg(t)

	p := NewPlayer(600)
	defer p.stopDecoder()

	videoPath := createIntegrationVideo(t)
	if err := p.Load(videoPath); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.TotalFrames > 0 && !s.Playing
	})

	expectedFrame1 := seekAndCaptureFrame(t, p, 1)

	stream := p.SubscribeStream()
	defer p.UnsubscribeStream(stream)
	drainStream(stream)

	p.Play()
	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.Playing && s.CurrentFrame >= 30
	})
	time.Sleep(300 * time.Millisecond)

	p.Pause()
	waitForState(t, p, 3*time.Second, func(s State) bool {
		return !s.Playing
	})

	p.Seek(1)
	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.CurrentFrame == 1 && !s.Playing
	})

	got := readStreamFrame(t, stream, 3*time.Second)
	if !bytes.Equal(got, expectedFrame1) {
		t.Fatalf(
			"buffered stream did not publish latest seek frame first: got=%s expected=%s",
			shortHash(got),
			shortHash(expectedFrame1),
		)
	}
}

func TestFirstPrevAfterPauseShowsPreviousFrame(t *testing.T) {
	skipIfNoFFmpeg(t)

	p := NewPlayer(600)
	defer p.stopDecoder()

	videoPath := createIntegrationVideo(t)
	if err := p.Load(videoPath); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.TotalFrames > 0 && !s.Playing
	})

	stream := p.SubscribeStream()
	defer p.UnsubscribeStream(stream)
	drainStream(stream)

	p.Play()
	waitForState(t, p, 5*time.Second, func(s State) bool {
		return s.Playing && s.CurrentFrame >= 35
	})
	time.Sleep(300 * time.Millisecond)

	p.Pause()
	pausedState := waitForState(t, p, 3*time.Second, func(s State) bool {
		return !s.Playing
	})
	if pausedState.CurrentFrame < 1 {
		t.Fatalf("unexpected paused frame: %d", pausedState.CurrentFrame)
	}

	target := pausedState.CurrentFrame - 1
	expected, ok := p.getCache(target)
	if !ok {
		p.fetchChunk(target, target)
		expected, ok = p.getCache(target)
		if !ok {
			t.Fatalf("target frame %d was not available in cache", target)
		}
	}
	expected = cloneBytes(expected)

	p.Step(-1)
	waitForState(t, p, 5*time.Second, func(s State) bool {
		return !s.Playing && s.CurrentFrame == target
	})

	got := readStreamFrame(t, stream, 3*time.Second)
	if !bytes.Equal(got, expected) {
		t.Fatalf(
			"first prev after pause did not render previous frame: target=%d got=%s expected=%s",
			target,
			shortHash(got),
			shortHash(expected),
		)
	}
}

func skipIfNoFFmpeg(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("ffmpeg not found in PATH: %v", err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skipf("ffprobe not found in PATH: %v", err)
	}
}

func createIntegrationVideo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "integration-sample.mp4")
	cmd := exec.Command(
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-f", "lavfi",
		"-i", "testsrc2=size=320x180:rate=30",
		"-t", "6",
		"-pix_fmt", "yuv420p",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to create integration video: %v, output: %s", err, string(out))
	}
	return path
}

func seekAndCaptureFrame(t *testing.T, p *Player, frame int) []byte {
	t.Helper()
	p.Seek(frame)
	waitForState(t, p, 5*time.Second, func(s State) bool {
		return !s.Playing && s.CurrentFrame == frame
	})
	img := cloneBytes(p.GetCurrentImage())
	if len(img) == 0 {
		t.Fatalf("current image is empty at frame %d", frame)
	}
	return img
}

func waitForState(t *testing.T, p *Player, timeout time.Duration, predicate func(State) bool) State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s := p.GetState()
		if predicate(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := p.GetState()
	t.Fatalf("timed out waiting for state; last state: %+v", s)
	return State{}
}

func readStreamFrame(t *testing.T, stream <-chan []byte, timeout time.Duration) []byte {
	t.Helper()
	select {
	case img := <-stream:
		return cloneBytes(img)
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for stream frame after %s", timeout)
		return nil
	}
}

func drainStream(stream <-chan []byte) {
	for {
		select {
		case <-stream:
		default:
			return
		}
	}
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	out := make([]byte, len(src))
	copy(out, src)
	return out
}

func shortHash(b []byte) string {
	if len(b) == 0 {
		return "empty"
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%s (%d bytes)", hex.EncodeToString(sum[:8]), len(b))
}
