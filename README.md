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
- **High-Performance Scrubbing**: A bidirectional LRU frame cache ensures perfectly instantaneous backward stepping, backward playback, and scrubbing without launching new decoder processes per frame.
- **Infinite Loop Playback**: Playback wraps automatically at boundaries (end -> start, start -> end) instead of stopping.
- **Graceful Frame-Skipping**: If playback is configured higher than system decoding capabilities (e.g., 240 fps), the backend automatically drops MJPEG broadcasts while maintaining precise logical frame advancement (render one, skip three).

## Usage

1. Start the server:
   ```bash
   go run .
   ```
   *Available CLI arguments:*
   ```bash
   go run . -file /absolute/path/to/video.mkv -port 8080 -cache 1000
   ```
   - `-file`: Load a video immediately on startup.
   - `-port`: The HTTP server port (default: 8080).
   - `-cache`: Number of decoded MJPEG frames to keep in memory for instantaneous scrubbing and backward playback (default: 1000).

2. Open your browser to `http://localhost:8080`.

3. **Hotkeys**:
   - `Space`: Play/Pause
   - `Right Arrow`: Step Forward 1 frame
   - `Left Arrow`: Step Backward 1 frame

## Architecture

- `main.go`: Handles HTTP routing, WebSocket/SSE endpoints, and the MJPEG stream.
- `player.go`: The core playback engine controlling an external `ffmpeg` process. It loops over precise time tickers, reads raw JPEGs via pipe from `ffmpeg`, tracks frame positions, and handles dynamic seeking and scaling.
- `static/`: Pure HTML, JS, and CSS. No build step required.
