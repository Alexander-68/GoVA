package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type State struct {
	File          string  `json:"file"`
	Playing       bool    `json:"playing"`
	PlayDirection int     `json:"playDirection"`
	FPS           float64 `json:"fps"`
	PlayFPS       float64 `json:"playFPS"`
	Duration      float64 `json:"duration"`
	TotalFrames   int     `json:"totalFrames"`
	CurrentFrame  int     `json:"currentFrame"`
}

type prefetchJob struct {
	gen   int
	start int
	end   int
	key   string
}

type Player struct {
	mu           sync.RWMutex
	state        State
	cmd          *exec.Cmd
	stdoutReader io.ReadCloser

	playChan chan bool
	seekChan chan int
	stepChan chan int
	fpsChan  chan float64

	frameChan    chan []byte
	currentImage []byte
	decoderWG    sync.WaitGroup
	decoderStop  chan struct{}

	streamSubs map[chan []byte]struct{}
	stateSubs  map[chan State]struct{}

	maxCacheSize int
	cache        map[int][]byte
	cacheKeys    []int
	cacheMu      sync.RWMutex

	prefetchMu            sync.Mutex
	reversePrefetchJobs   chan prefetchJob
	reversePrefetchQueued map[string]struct{}
	reversePrefetchGen    int
	reverseChunkSize      int
	reverseChunkBuffers   int
}

func NewPlayer(maxCacheSize int) *Player {
	reverseChunkBuffers := 3
	reverseChunkSize := 180
	if maxCacheSize > 0 {
		reverseChunkSize = maxCacheSize / reverseChunkBuffers
		if reverseChunkSize < 90 {
			reverseChunkSize = 90
		}
		if reverseChunkSize > maxCacheSize {
			reverseChunkSize = maxCacheSize
		}
	}

	p := &Player{
		state: State{
			PlayFPS:       30.0,
			PlayDirection: 1,
		},
		playChan:              make(chan bool, 1),
		seekChan:              make(chan int, 1),
		stepChan:              make(chan int, 1),
		fpsChan:               make(chan float64, 1),
		frameChan:             make(chan []byte, 100),
		streamSubs:            make(map[chan []byte]struct{}),
		stateSubs:             make(map[chan State]struct{}),
		maxCacheSize:          maxCacheSize,
		cache:                 make(map[int][]byte),
		cacheKeys:             make([]int, 0, maxCacheSize),
		reversePrefetchJobs:   make(chan prefetchJob, 24),
		reversePrefetchQueued: make(map[string]struct{}),
		reverseChunkSize:      reverseChunkSize,
		reverseChunkBuffers:   reverseChunkBuffers,
	}
	go p.reversePrefetchWorker()
	go p.loop()
	return p
}

func (p *Player) setCache(frame int, img []byte) {
	if p.maxCacheSize <= 0 {
		return
	}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	if _, exists := p.cache[frame]; !exists {
		if len(p.cacheKeys) >= p.maxCacheSize {
			oldest := p.cacheKeys[0]
			p.cacheKeys = p.cacheKeys[1:]
			delete(p.cache, oldest)
		}
		p.cacheKeys = append(p.cacheKeys, frame)
	}
	p.cache[frame] = img
}

func (p *Player) getCache(frame int) ([]byte, bool) {
	p.cacheMu.RLock()
	defer p.cacheMu.RUnlock()
	img, ok := p.cache[frame]
	return img, ok
}

func (p *Player) clearCache() {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.cache = make(map[int][]byte)
	p.cacheKeys = make([]int, 0, p.maxCacheSize)
}

func modFrame(frame, totalFrames int) int {
	if totalFrames <= 0 {
		return 0
	}
	frame %= totalFrames
	if frame < 0 {
		frame += totalFrames
	}
	return frame
}

func (p *Player) isRangeCached(startFrame, endFrame int) bool {
	if startFrame > endFrame {
		return true
	}

	p.cacheMu.RLock()
	defer p.cacheMu.RUnlock()
	for frame := startFrame; frame <= endFrame; frame++ {
		if _, ok := p.cache[frame]; !ok {
			return false
		}
	}
	return true
}

func (p *Player) resetReversePrefetch() {
	p.prefetchMu.Lock()
	p.reversePrefetchGen++
	p.reversePrefetchQueued = make(map[string]struct{})
	p.prefetchMu.Unlock()

	for {
		select {
		case <-p.reversePrefetchJobs:
		default:
			return
		}
	}
}

func (p *Player) queueReversePrefetch(startFrame, endFrame, gen int) {
	if startFrame > endFrame || p.isRangeCached(startFrame, endFrame) {
		return
	}

	key := fmt.Sprintf("%d:%d-%d", gen, startFrame, endFrame)

	p.prefetchMu.Lock()
	if gen != p.reversePrefetchGen {
		p.prefetchMu.Unlock()
		return
	}
	if _, exists := p.reversePrefetchQueued[key]; exists {
		p.prefetchMu.Unlock()
		return
	}
	p.reversePrefetchQueued[key] = struct{}{}
	p.prefetchMu.Unlock()

	select {
	case p.reversePrefetchJobs <- prefetchJob{
		gen:   gen,
		start: startFrame,
		end:   endFrame,
		key:   key,
	}:
	default:
		p.prefetchMu.Lock()
		delete(p.reversePrefetchQueued, key)
		p.prefetchMu.Unlock()
	}
}

func (p *Player) scheduleDirectionalPrefetch(anchorFrame, totalFrames, direction, buffers int) {
	if p.maxCacheSize <= 0 || totalFrames <= 0 {
		return
	}
	if direction == 0 {
		direction = 1
	}

	chunkSize := p.reverseChunkSize
	if chunkSize < 1 {
		chunkSize = 1
	}
	if buffers < 1 {
		buffers = 1
	}
	if buffers > p.reverseChunkBuffers {
		buffers = p.reverseChunkBuffers
	}
	if buffers < 1 {
		buffers = 1
	}
	if p.maxCacheSize > 0 {
		cacheLimitedBuffers := p.maxCacheSize / chunkSize
		if cacheLimitedBuffers < 1 {
			cacheLimitedBuffers = 1
		}
		if buffers > cacheLimitedBuffers {
			buffers = cacheLimitedBuffers
		}
	}

	p.prefetchMu.Lock()
	gen := p.reversePrefetchGen
	p.prefetchMu.Unlock()

	anchorFrame = modFrame(anchorFrame, totalFrames)
	chunkCount := (totalFrames + chunkSize - 1) / chunkSize
	if chunkCount < 1 {
		chunkCount = 1
	}
	if buffers > chunkCount {
		buffers = chunkCount
	}
	baseChunk := anchorFrame / chunkSize

	step := 1
	if direction < 0 {
		step = -1
	}
	preferred := []int{
		modFrame(baseChunk+step, chunkCount), // direction-first
		baseChunk,                            // current
		modFrame(baseChunk-step, chunkCount), // opposite side
	}

	seen := make(map[int]struct{}, len(preferred))
	queued := 0
	for _, chunkIdx := range preferred {
		if queued >= buffers {
			break
		}
		if _, exists := seen[chunkIdx]; exists {
			continue
		}
		seen[chunkIdx] = struct{}{}
		chunkStart := chunkIdx * chunkSize
		chunkEnd := chunkStart + chunkSize - 1
		if chunkEnd >= totalFrames {
			chunkEnd = totalFrames - 1
		}
		p.queueReversePrefetch(chunkStart, chunkEnd, gen)
		queued++
	}
}

func (p *Player) waitForCachedFrame(frame int, timeout time.Duration) ([]byte, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if img, ok := p.getCache(frame); ok {
			return img, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(4 * time.Millisecond)
	}
}

func (p *Player) reversePrefetchWorker() {
	for job := range p.reversePrefetchJobs {
		p.prefetchMu.Lock()
		currentGen := p.reversePrefetchGen
		p.prefetchMu.Unlock()

		if job.gen != currentGen {
			continue
		}

		if !p.isRangeCached(job.start, job.end) {
			p.fetchChunk(job.start, job.end)
		}

		p.prefetchMu.Lock()
		if job.gen == p.reversePrefetchGen {
			delete(p.reversePrefetchQueued, job.key)
		}
		p.prefetchMu.Unlock()
	}
}

func (p *Player) Load(file string) error {
	file = strings.Trim(strings.TrimSpace(file), "\"")
	file = filepath.Clean(file)
	log.Printf("Load: Cleaned file path: %s", file)
	if _, err := os.Stat(file); os.IsNotExist(err) {
		log.Printf("Load: File does not exist at %s: %v", file, err)
		return fmt.Errorf("file does not exist: %s", file)
	} else if err != nil {
		log.Printf("Load: Error checking file %s: %v", file, err)
		return fmt.Errorf("error accessing file: %v", err)
	}

	log.Printf("Load: Attempting to get metadata for %s", file)
	fps, duration, err := getMetadata(file)
	if err != nil {
		log.Printf("Load: ffprobe failed for %s: %v", file, err)
		return fmt.Errorf("ffprobe failed: %v", err)
	}
	log.Printf("Load: Metadata retrieved - FPS: %f, Duration: %f", fps, duration)

	totalFrames := int(duration * fps)

	p.clearCache()
	p.resetReversePrefetch()

	p.mu.Lock()
	p.state.File = file
	p.state.FPS = fps
	p.state.PlayFPS = fps
	p.state.Duration = duration
	p.state.TotalFrames = totalFrames
	p.state.CurrentFrame = 0
	p.state.Playing = false
	p.state.PlayDirection = 1
	p.mu.Unlock()

	if totalFrames > 0 && totalFrames <= p.maxCacheSize {
		log.Printf("Load: Video fits in cache (%d <= %d), starting full prefetch", totalFrames, p.maxCacheSize)
		go p.fetchChunk(0, totalFrames-1)
	}

	p.seekChan <- 0
	return nil
}

func (p *Player) Play() {
	p.mu.Lock()
	p.state.PlayDirection = 1
	p.mu.Unlock()
	p.resetReversePrefetch()
	p.playChan <- true
}

func (p *Player) PlayBackward() {
	p.mu.Lock()
	p.state.PlayDirection = -1
	p.mu.Unlock()
	p.resetReversePrefetch()
	p.playChan <- true
}

func (p *Player) Pause() {
	p.playChan <- false
}

func (p *Player) Step(frames int) {
	p.resetReversePrefetch()
	p.stepChan <- frames
}

func (p *Player) Seek(frame int) {
	p.resetReversePrefetch()
	p.seekChan <- frame
}

func (p *Player) SetFPS(fps float64) {
	if fps > 0 && fps <= 240 {
		p.fpsChan <- fps
	}
}

func (p *Player) fetchChunk(startFrame, endFrame int) {
	p.mu.RLock()
	file := p.state.File
	fps := p.state.FPS
	p.mu.RUnlock()

	if file == "" || startFrame > endFrame {
		return
	}

	timeSec := float64(startFrame) / fps
	frameCount := endFrame - startFrame + 1

	cmd := exec.Command("ffmpeg",
		"-ss", fmt.Sprintf("%.3f", timeSec),
		"-i", file,
		"-frames:v", strconv.Itoa(frameCount),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-vf", "scale=1280:720:force_original_aspect_ratio=decrease",
		"-q:v", "2",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("fetchChunk stdout pipe error: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("fetchChunk start error: %v", err)
		return
	}

	p.parseJPEGsAndCache(stdout, startFrame, nil, nil)
	cmd.Wait()
}

func (p *Player) parseJPEGsAndCache(r io.Reader, startFrame int, frameChan chan<- []byte, stop <-chan struct{}) {
	buf := make([]byte, 1024*1024)
	n := 0
	currentFrame := startFrame
	for {
		if stop != nil {
			select {
			case <-stop:
				return
			default:
			}
		}

		m, err := r.Read(buf[n:])
		n += m

		for {
			soi := bytes.Index(buf[:n], []byte{0xFF, 0xD8})
			if soi == -1 {
				if n > 0 {
					copy(buf, buf[n-1:])
					n = 1
				}
				break
			}
			eoi := bytes.Index(buf[soi:], []byte{0xFF, 0xD9})
			if eoi == -1 {
				copy(buf, buf[soi:])
				n = n - soi
				break
			}
			eoi += soi + 2

			frameBytes := make([]byte, eoi-soi)
			copy(frameBytes, buf[soi:eoi])

			p.setCache(currentFrame, frameBytes)
			currentFrame++

			if frameChan != nil {
				if stop != nil {
					select {
					case frameChan <- frameBytes:
					case <-stop:
						return
					}
				} else {
					frameChan <- frameBytes
				}
			}

			copy(buf, buf[eoi:])
			n = n - eoi
		}

		if err != nil {
			break
		}
	}
}

func (p *Player) loop() {
	var lastTick time.Time
	var idlePrefetchAnchor int = -1
	var idlePrefetchDir int = 1
	var idlePrefetchSince time.Time
	var idlePrimaryRefresh time.Time
	var idleOppositeQueued bool
	seekAndRender := func(targetFrame int, resumePlaying bool) {
		p.mu.Lock()
		if p.state.File == "" || p.state.TotalFrames <= 0 {
			p.state.Playing = false
			p.mu.Unlock()
			p.broadcastState()
			return
		}
		if targetFrame < 0 {
			targetFrame = 0
		}
		if targetFrame >= p.state.TotalFrames {
			targetFrame = p.state.TotalFrames - 1
		}
		p.mu.Unlock()

		if img, ok := p.getCache(targetFrame); ok {
			p.mu.Lock()
			p.state.CurrentFrame = targetFrame
			p.currentImage = img
			p.state.Playing = resumePlaying
			if resumePlaying {
				lastTick = time.Now()
			}
			p.mu.Unlock()

			p.broadcastImage(img)
			if !resumePlaying {
				p.rebroadcastPausedFrame(targetFrame, img)
			}
			p.broadcastState()

			p.mu.RLock()
			playDir := p.state.PlayDirection
			totalFrames := p.state.TotalFrames
			isFullyCached := totalFrames > 0 && p.isRangeCached(0, totalFrames-1)
			p.mu.RUnlock()

			if resumePlaying {
				if playDir > 0 {
					if !isFullyCached {
						p.restartFFmpeg(targetFrame)
					}
				} else if playDir < 0 {
					p.scheduleDirectionalPrefetch(targetFrame, totalFrames, playDir, 2)
				}
			}
			return
		}

		p.restartFFmpeg(targetFrame)

		img, ok := p.readFrame(2 * time.Second)

		p.mu.Lock()
		p.state.CurrentFrame = targetFrame
		if ok {
			p.currentImage = img
		}
		p.state.Playing = resumePlaying && ok
		if p.state.Playing {
			lastTick = time.Now()
		}
		p.mu.Unlock()

		if ok && img != nil {
			p.broadcastImage(img)
			if !resumePlaying {
				p.rebroadcastPausedFrame(targetFrame, img)
			}
		}
		p.broadcastState()

		if ok && resumePlaying {
			p.mu.RLock()
			playDir := p.state.PlayDirection
			totalFrames := p.state.TotalFrames
			p.mu.RUnlock()
			if playDir < 0 {
				p.scheduleDirectionalPrefetch(targetFrame, totalFrames, playDir, 2)
			}
		}
	}

	for {
		select {
		case fps := <-p.fpsChan:
			p.mu.Lock()
			p.state.PlayFPS = fps
			p.mu.Unlock()
			p.broadcastState()

		case play := <-p.playChan:
			p.mu.Lock()
			currentFrame := p.state.CurrentFrame
			totalFrames := p.state.TotalFrames
			playDir := p.state.PlayDirection
			if p.state.File != "" {
				p.state.Playing = play
				if play {
					lastTick = time.Now()
				}
			}
			p.mu.Unlock()

			if !play {
				p.stopDecoder()
			}

			if play && playDir < 0 {
				p.scheduleDirectionalPrefetch(currentFrame, totalFrames, playDir, 2)
			}
			p.broadcastState()

		case targetFrame := <-p.seekChan:
			p.mu.Lock()
			if p.state.File == "" {
				p.mu.Unlock()
				continue
			}
			wasPlaying := p.state.Playing
			p.state.Playing = false
			p.mu.Unlock()

			seekAndRender(targetFrame, wasPlaying)

		case delta := <-p.stepChan:
			p.mu.Lock()
			if p.state.File == "" {
				p.mu.Unlock()
				continue
			}
			p.state.Playing = false
			target := p.state.CurrentFrame + delta
			if target < 0 {
				target = 0
			}
			if target >= p.state.TotalFrames {
				target = p.state.TotalFrames - 1
			}
			p.mu.Unlock()

			if delta < 0 {
				if _, ok := p.getCache(target); !ok {
					stepPrefetchSize := p.reverseChunkSize
					if stepPrefetchSize > 180 {
						stepPrefetchSize = 180
					}
					chunkStart := target - stepPrefetchSize + 1
					if chunkStart < 0 {
						chunkStart = 0
					}
					p.fetchChunk(chunkStart, target)
				}
			}

			seekAndRender(target, false)

		default:
			p.mu.Lock()
			playing := p.state.Playing
			playDirection := p.state.PlayDirection
			playFPS := p.state.PlayFPS
			file := p.state.File
			currentFrame := p.state.CurrentFrame
			totalFrames := p.state.TotalFrames
			p.mu.Unlock()

			if !playing || file == "" {
				if !playing && file != "" && totalFrames > 0 {
					now := time.Now()
					if playDirection == 0 {
						playDirection = 1
					}
					anchorChanged := idlePrefetchAnchor != currentFrame || idlePrefetchDir != playDirection

					if anchorChanged || idlePrefetchSince.IsZero() {
						idlePrefetchAnchor = currentFrame
						idlePrefetchDir = playDirection
						idlePrefetchSince = now
						idlePrimaryRefresh = now
						idleOppositeQueued = false
						p.scheduleDirectionalPrefetch(currentFrame, totalFrames, playDirection, 2)
					} else {
						if now.Sub(idlePrimaryRefresh) >= 350*time.Millisecond {
							p.scheduleDirectionalPrefetch(currentFrame, totalFrames, playDirection, 2)
							idlePrimaryRefresh = now
						}
						if !idleOppositeQueued && now.Sub(idlePrefetchSince) >= 750*time.Millisecond {
							p.scheduleDirectionalPrefetch(currentFrame, totalFrames, -playDirection, 1)
							idleOppositeQueued = true
						}
					}
				} else {
					idlePrefetchAnchor = -1
					idlePrefetchSince = time.Time{}
					idleOppositeQueued = false
				}
				time.Sleep(10 * time.Millisecond)
				continue
			}
			idlePrefetchAnchor = -1
			idlePrefetchSince = time.Time{}
			idleOppositeQueued = false

			now := time.Now()
			elapsed := now.Sub(lastTick).Seconds()
			expectedFrames := int(elapsed * playFPS)

			if expectedFrames > 0 {
				lastTick = now

				if expectedFrames > 30 {
					expectedFrames = 1
				}

				if totalFrames <= 0 {
					p.mu.Lock()
					p.state.Playing = false
					p.mu.Unlock()
					p.broadcastState()
					continue
				}

				if playDirection < 0 {
					target := currentFrame - expectedFrames
					target %= totalFrames
					if target < 0 {
						target += totalFrames
					}

					p.scheduleDirectionalPrefetch(target, totalFrames, playDirection, 2)

					rendered := false
					if img, ok := p.getCache(target); ok {
						p.mu.Lock()
						p.state.CurrentFrame = target
						p.currentImage = img
						p.mu.Unlock()
						p.broadcastImage(img)
						rendered = true
					}
					if !rendered {
						if img, ok := p.waitForCachedFrame(target, 120*time.Millisecond); ok {
							p.mu.Lock()
							p.state.CurrentFrame = target
							p.currentImage = img
							p.mu.Unlock()
							p.broadcastImage(img)
							rendered = true
						}
					}
					if !rendered {
						// Minimal synchronous recovery for rare misses while background prefetch catches up.
						p.fetchChunk(target, target)
						if img, ok := p.getCache(target); ok {
							p.mu.Lock()
							p.state.CurrentFrame = target
							p.currentImage = img
							p.mu.Unlock()
							p.broadcastImage(img)
							rendered = true
						}
					}
					if rendered {
						p.broadcastState()
						continue
					}

					seekAndRender(target, true)
					continue
				}

				var img []byte
				loopToStart := false
				stalled := false
				for i := 0; i < expectedFrames; i++ {
					p.mu.RLock()
					atEnd := p.state.CurrentFrame >= p.state.TotalFrames-1
					nextFrame := p.state.CurrentFrame + 1
					p.mu.RUnlock()
					if atEnd {
						loopToStart = true
						break
					}

					if cachedImg, ok := p.getCache(nextFrame); ok {
						img = cachedImg
						p.mu.Lock()
						p.state.CurrentFrame = nextFrame
						p.currentImage = img
						p.mu.Unlock()
						continue
					}

					frame, ok := p.readFrame(750 * time.Millisecond)
					if !ok {
						stalled = true
						break
					}
					img = frame
					p.mu.Lock()
					p.state.CurrentFrame++
					p.currentImage = img
					p.mu.Unlock()
				}

				if img != nil {
					p.broadcastImage(img)
				}
				p.broadcastState()

				if loopToStart {
					seekAndRender(0, true)
				} else if stalled {
					p.mu.RLock()
					cur := p.state.CurrentFrame
					total := p.state.TotalFrames
					p.mu.RUnlock()

					if total > 1 && cur >= total-2 {
						seekAndRender(0, true)
					} else {
						seekAndRender(cur+1, true)
					}
				}
			} else {
				time.Sleep(2 * time.Millisecond)
			}
		}
	}
}

func (p *Player) readFrame(timeout time.Duration) ([]byte, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case img := <-p.frameChan:
		return img, true
	case <-timer.C:
		return nil, false
	}
}

func (p *Player) stopDecoder() {
	if p.decoderStop != nil {
		close(p.decoderStop)
		p.decoderStop = nil
	}

	if p.cmd != nil {
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
		p.cmd.Wait()
		if p.stdoutReader != nil {
			p.stdoutReader.Close()
		}
		p.cmd = nil
		p.stdoutReader = nil
	}

	p.decoderWG.Wait()

drain:
	for {
		select {
		case <-p.frameChan:
		default:
			break drain
		}
	}
}

func (p *Player) restartFFmpeg(frame int) {
	if _, ok := p.getCache(frame); ok {
		return
	}

	p.stopDecoder()

	p.mu.RLock()
	file := p.state.File
	fps := p.state.FPS
	p.mu.RUnlock()

	timeSec := float64(frame) / fps

	cmd := exec.Command("ffmpeg",
		"-ss", fmt.Sprintf("%.3f", timeSec),
		"-i", file,
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-vf", "scale=1280:720:force_original_aspect_ratio=decrease",
		"-q:v", "2",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create stdout pipe: %v", err)
		return
	}
	p.stdoutReader = stdout
	p.cmd = cmd

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start ffmpeg: %v", err)
		p.cmd = nil
		p.stdoutReader = nil
		return
	}

	decoderStop := make(chan struct{})
	p.decoderStop = decoderStop

	p.decoderWG.Add(1)
	go func() {
		defer p.decoderWG.Done()
		p.parseJPEGsAndCache(stdout, frame, p.frameChan, decoderStop)
	}()
}

func getMetadata(file string) (float64, float64, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate,duration",
		"-of", "csv=p=0",
		file,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("ffprobe command failed: %v, output: %s", err, string(out))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return 0, 0, fmt.Errorf("unexpected ffprobe output (no lines): %s", string(out))
	}

	fields := strings.Split(lines[0], ",")
	if len(fields) < 1 {
		return 0, 0, fmt.Errorf("unexpected fields: %s", lines[0])
	}

	fpsField := strings.TrimSpace(fields[0])
	fpsStr := strings.Split(fpsField, "/")
	var fps float64
	if len(fpsStr) == 2 {
		num, _ := strconv.ParseFloat(strings.TrimSpace(fpsStr[0]), 64)
		den, _ := strconv.ParseFloat(strings.TrimSpace(fpsStr[1]), 64)
		if den != 0 {
			fps = num / den
		}
	} else if len(fpsStr) == 1 {
		fps, _ = strconv.ParseFloat(strings.TrimSpace(fpsStr[0]), 64)
	}

	var duration float64
	if len(fields) > 1 {
		duration, _ = strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
	}

	if duration == 0 {
		log.Printf("getMetadata: Duration is 0, trying fallback ffprobe for %s", file)
		cmd = exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file)
		out, err = cmd.CombinedOutput()
		if err == nil {
			duration, _ = strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
			log.Printf("getMetadata: Fallback duration: %f", duration)
		} else {
			log.Printf("getMetadata: Fallback ffprobe failed: %v, output: %s", err, string(out))
		}
	}

	if fps == 0 || duration == 0 {
		return 0, 0, fmt.Errorf("could not determine fps or duration")
	}

	return fps, duration, nil
}

// Subscriptions
func (p *Player) SubscribeStream() chan []byte {
	ch := make(chan []byte, 2)
	p.mu.Lock()
	p.streamSubs[ch] = struct{}{}
	p.mu.Unlock()
	return ch
}

func (p *Player) UnsubscribeStream(ch chan []byte) {
	p.mu.Lock()
	delete(p.streamSubs, ch)
	p.mu.Unlock()
}

func (p *Player) SubscribeState() chan State {
	ch := make(chan State, 2)
	p.mu.Lock()
	p.stateSubs[ch] = struct{}{}
	p.mu.Unlock()
	return ch
}

func (p *Player) UnsubscribeState(ch chan State) {
	p.mu.Lock()
	delete(p.stateSubs, ch)
	p.mu.Unlock()
}

func (p *Player) broadcastImage(img []byte) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for ch := range p.streamSubs {
		drained := false
		for !drained {
			select {
			case <-ch:
			default:
				drained = true
			}
		}
		select {
		case ch <- img:
		default:
		}
	}
}

func (p *Player) rebroadcastPausedFrame(frame int, img []byte) {
	if img == nil {
		return
	}

	go func(expectedFrame int, frameBytes []byte) {
		time.Sleep(24 * time.Millisecond)

		p.mu.RLock()
		stillPausedOnFrame := !p.state.Playing && p.state.CurrentFrame == expectedFrame
		p.mu.RUnlock()
		if !stillPausedOnFrame {
			return
		}

		p.broadcastImage(frameBytes)
	}(frame, img)
}

func (p *Player) broadcastState() {
	p.mu.RLock()
	state := p.state
	p.mu.RUnlock()

	p.mu.RLock()
	defer p.mu.RUnlock()
	for ch := range p.stateSubs {
		select {
		case ch <- state:
		default:
		}
	}
}

func (p *Player) GetState() State {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

func (p *Player) GetCurrentImage() []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.currentImage
}
