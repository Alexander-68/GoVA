GoVA: VideoAnnotate is a Go + web UI tool for annotating events on top of backend-controlled video playback. Simple, minimalistic approach to code, keep folder tree to reasonable minimum.

Selected video can be located anywhere in the filesystem, do not make special folder for videos.

Playback authority is in Go: frame position, play, pause, framerate, seek, and step are all controlled by the backend. Backend-rendered preview stream (MJPEG) with resolution capped to 1280x720 for landscape or 405x720 for portrait videos, all without audio. ffmpeg version 8.0.1 or higher and ffprobe are available in PATH. 

Video position is displayed and controlled by a frame number. FPS can be set to any value from 1 to 240 with default at actual video fps. When backend or frontend are not able to play high fps - skip frames, like render one frame, skip three.

This app code uses Go version 1.26 or newer. Use new Go features, do not care for compatibility with older Go versions.

Maintain README.md file updated with description and functionality for user.

When during evaluating task you find something unclear or inconsistent - ask me for confirmation before implementing code.

Extra tools available to agents on Windows and Linux platforms: Powershell 7.5, ripgrep 13.0. When external test/tool scripts are required, use PowerShell for cross-system compatibility.
