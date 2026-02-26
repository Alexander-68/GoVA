# GoVA: VideoAnnotate

GoVA is a minimal, frame-accurate video annotation tool powered by a Go backend and a lightweight Vanilla JS frontend. It uses an MJPEG stream generated locally by `ffmpeg` to ensure the backend maintains absolute playback authority.

## Requirements

- **Go**: 1.26+
- **ffmpeg** / **ffprobe**: 8.0.1+ (must be available in your system `PATH`)

## Features

- **Backend-Driven Playback**: The Go backend determines the precise frame position. The browser acts merely as a thin display layer.
- **Anywhere in filesystem**: Load video files by passing an absolute path from the CLI or via the web UI.
- **Responsive Controls**: Seek via frame number, step accurately by individual frames (-1 / +1), and dynamic framerate overrides (1 to 240 fps).
- **Bidirectional Playback**: Play forward or backward from the backend, including reverse-play button support in the UI.
- **High-Performance Scrubbing**: A bidirectional LRU frame cache ensures instantaneous stepping and scrubbing without launching new decoder processes per frame.
- **Smooth Reverse Playback**: Reverse playback uses a chunk-aligned triple-buffer prefetch queue in the background (including across start/end wrap), so chunk decoding happens ahead of time instead of stalling the playback loop at cache boundaries.
- **Directional Idle Prefetch**: While paused (including after load and slider seeks), prefetch starts in the expected play direction first, then fills the opposite direction after a short delay so playback can resume smoothly either way.
- **Direction-Scoped Active Prefetch**: While actively playing, prefetch work is constrained to the active playback direction to avoid wasting decode budget.
- **Infinite Loop Playback**: Playback wraps automatically at boundaries (end -> start, start -> end) instead of stopping.
- **Graceful Frame-Skipping**: If playback is configured higher than system decoding capabilities (e.g., 240 fps), the backend automatically drops MJPEG broadcasts while maintaining precise logical frame advancement (render one, skip three).

## Prefetch Strategy

- Prefetch works in fixed chunks (default cache `3000` -> three chunks of `1000` frames).
- Chunk addressing is circular, so prefetch naturally crosses the start/end boundary.
- While paused (including immediately after load and after slider seeks):
  - First, prefetch in the expected direction (`PlayDirection`) to reduce start latency.
  - If still paused after a short delay, prefetch one chunk in the opposite direction so either play direction is ready.
- While playing, prefetch is constrained to the active play direction only.

## Usage

1. Start the server:
   ```bash
   go run .
   ```
   *Available CLI arguments:*
   ```bash
   go run . -file /absolute/path/to/video.mkv -port 8080 -cache 3000
   ```
   - `-file`: Load a video immediately on startup.
   - `-port`: The HTTP server port (default: 8080).
   - `-cache`: Number of decoded MJPEG frames to keep in memory for instantaneous scrubbing and backward playback (default: 3000, typically split into three 1000-frame reverse-prefetch chunks).

2. Open your browser to `http://localhost:8080`.

3. **Hotkeys**:
   - `Space`: Play/Pause
   - `Right Arrow`: Step Forward 1 frame
   - `Left Arrow`: Step Backward 1 frame

## Architecture

- `main.go`: Handles HTTP routing, WebSocket/SSE endpoints, and the MJPEG stream.
- `player.go`: The core playback engine controlling an external `ffmpeg` process. It loops over precise time tickers, reads raw JPEGs via pipe from `ffmpeg`, tracks frame positions, and handles dynamic seeking and scaling.
- `static/`: Pure HTML, JS, and CSS. No build step required.
