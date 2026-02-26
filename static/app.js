document.addEventListener('DOMContentLoaded', () => {
    const fileInput = document.getElementById('fileInput');
    const loadBtn = document.getElementById('loadBtn');
    
    const videoStream = document.getElementById('videoStream');
    const playPauseBtn = document.getElementById('playPauseBtn');
    const playBackBtn = document.getElementById('playBackBtn');
    const stepBackBtn = document.getElementById('stepBackBtn');
    const stepFwdBtn = document.getElementById('stepFwdBtn');
    
    const frameInput = document.getElementById('frameInput');
    const frameSlider = document.getElementById('frameSlider');
    const totalFramesDisplay = document.getElementById('totalFramesDisplay');
    
    const fpsInput = document.getElementById('fpsInput');
    const setFpsBtn = document.getElementById('setFpsBtn');
    
    const statusDiv = document.getElementById('status');
    
    let isPlaying = false;
    let playDirection = 1;
    let isSeeking = false;
    let totalFrames = 0;
    let currentFile = "";

    // Connect Server-Sent Events for state updates
    const stateSource = new EventSource('/state');
    
    stateSource.onmessage = (event) => {
        const state = JSON.parse(event.data);
        
        isPlaying = state.playing;
        playDirection = state.playDirection || 1;
        playPauseBtn.textContent = isPlaying ? '⏸' : '▶';
        playBackBtn.textContent = isPlaying && playDirection < 0 ? '⏸' : '◀';
        
        if (state.file && fileInput.value === '') {
            fileInput.value = state.file;
        }

        totalFrames = state.totalFrames;
        totalFramesDisplay.textContent = `/ ${state.totalFrames}`;
        frameSlider.max = state.totalFrames - 1;
        
        if (!isSeeking) {
            frameInput.value = state.currentFrame;
            frameSlider.value = state.currentFrame;
        }

        // Handle stream initialization or file change
        if (state.file && state.file !== currentFile) {
            currentFile = state.file;
            videoStream.src = `/stream?t=${Date.now()}`;
        }
        
        // If not actively typing in FPS input, update it
        if (document.activeElement !== fpsInput) {
            fpsInput.value = state.playFPS;
        }

        // FPS control visibility
        setFpsBtn.style.display = isPlaying ? 'none' : 'inline-block';
        fpsInput.disabled = isPlaying;
        
        const playbackDirection = playDirection < 0 ? 'Reverse' : 'Forward';
        statusDiv.textContent = `File: ${state.file || 'None'} | Native FPS: ${state.fps ? state.fps.toFixed(2) : 0} | Direction: ${playbackDirection}`;
    };

    // API commands
    const postCmd = (url, body) => fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams(body).toString()
    });

    loadBtn.onclick = () => {
        const file = fileInput.value.trim();
        if (file) {
            statusDiv.textContent = `Loading: ${file}...`;
            postCmd('/api/load', { file })
                .then(response => {
                    if (!response.ok) {
                        return response.text().then(text => {
                            statusDiv.textContent = `Error: ${text}`;
                            console.error('Load error:', text);
                        });
                    }
                })
                .catch(e => {
                    statusDiv.textContent = `Network Error: ${e.message}`;
                    console.error(e);
                });
        }
    };

    playPauseBtn.onclick = () => {
        if (isPlaying) {
            postCmd('/api/pause', {});
        } else {
            postCmd('/api/play', {});
        }
    };

    playBackBtn.onclick = () => {
        if (isPlaying && playDirection < 0) {
            postCmd('/api/pause', {});
        } else {
            postCmd('/api/play-backward', {});
        }
    };

    stepBackBtn.onclick = () => postCmd('/api/step', { frames: -1 });
    stepFwdBtn.onclick = () => postCmd('/api/step', { frames: 1 });

    const seekToFrame = (frame) => {
        if (frame >= 0 && frame < totalFrames) {
            postCmd('/api/seek', { frame });
        }
    };

    frameInput.onchange = (e) => seekToFrame(parseInt(e.target.value));
    
    frameSlider.oninput = (e) => {
        isSeeking = true;
        frameInput.value = e.target.value;
    };
    
    frameSlider.onchange = (e) => {
        seekToFrame(parseInt(e.target.value));
        isSeeking = false;
    };

    setFpsBtn.onclick = () => {
        const fps = parseFloat(fpsInput.value);
        if (fps > 0) {
            postCmd('/api/fps', { fps });
        }
    };

    // Keyboard shortcuts
    document.addEventListener('keydown', (e) => {
        // Prevent hotkeys if user is typing in inputs
        if (e.target.tagName === 'INPUT') return;
        
        if (e.code === 'Space') {
            e.preventDefault();
            playPauseBtn.click();
        } else if (e.code === 'ArrowRight') {
            e.preventDefault();
            stepFwdBtn.click();
        } else if (e.code === 'ArrowLeft') {
            e.preventDefault();
            stepBackBtn.click();
        }
    });
});
