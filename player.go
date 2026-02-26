package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
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

	streamSubs map[chan []byte]struct{}
	stateSubs  map[chan State]struct{}

	maxCacheSize int
	cache        map[int][]byte
	cacheKeys    []int
	cacheMu      sync.RWMutex
}

func NewPlayer(maxCacheSize int) *Player {
	p := &Player{
		state: State{
			PlayFPS:       30.0,
			PlayDirection: 1,
		},
		playChan:     make(chan bool, 1),
		seekChan:     make(chan int, 1),
		stepChan:     make(chan int, 1),
		fpsChan:      make(chan float64, 1),
		frameChan:    make(chan []byte, 100),
		streamSubs:   make(map[chan []byte]struct{}),
		stateSubs:    make(map[chan State]struct{}),
		maxCacheSize: maxCacheSize,
		cache:        make(map[int][]byte),
		cacheKeys:    make([]int, 0, maxCacheSize),
	}
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

func (p *Player) Load(file string) error {
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return fmt.Errorf("file does not exist: %s", file)
	}

	fps, duration, err := getMetadata(file)
	if err != nil {
		return fmt.Errorf("ffprobe failed: %v", err)
	}

	totalFrames := int(duration * fps)

	p.clearCache()

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

	p.seekChan <- 0
	return nil
}

func (p *Player) Play() {
	p.mu.Lock()
	p.state.PlayDirection = 1
	p.mu.Unlock()
	p.playChan <- true
}

func (p *Player) PlayBackward() {
	p.mu.Lock()
	p.state.PlayDirection = -1
	p.mu.Unlock()
	p.playChan <- true
}

func (p *Player) Pause() {
	p.playChan <- false
}

func (p *Player) Step(frames int) {
	p.stepChan <- frames
}

func (p *Player) Seek(frame int) {
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

	p.parseJPEGsAndCache(stdout, startFrame, nil)
	cmd.Wait()
}

func (p *Player) parseJPEGsAndCache(r io.Reader, startFrame int, frameChan chan<- []byte) {
	buf := make([]byte, 1024*1024)
	n := 0
	currentFrame := startFrame
	for {
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
				frameChan <- frameBytes
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
			p.broadcastState()

			p.mu.RLock()
			playDir := p.state.PlayDirection
			p.mu.RUnlock()

			if resumePlaying && playDir > 0 {
				p.restartFFmpeg(targetFrame)
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
		}
		p.broadcastState()
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
			if p.state.File != "" {
				p.state.Playing = play
				if play {
					lastTick = time.Now()
				}
			}
			p.mu.Unlock()
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
					chunkStart := target - 60
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
				time.Sleep(10 * time.Millisecond)
				continue
			}

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

					if img, ok := p.getCache(target); ok {
						p.mu.Lock()
						p.state.CurrentFrame = target
						p.currentImage = img
						p.mu.Unlock()
						p.broadcastImage(img)
						p.broadcastState()
					} else {
						chunkStart := target - 120
						if chunkStart < 0 {
							chunkStart = 0
						}
						p.fetchChunk(chunkStart, target)

						if img, ok := p.getCache(target); ok {
							p.mu.Lock()
							p.state.CurrentFrame = target
							p.currentImage = img
							p.mu.Unlock()
							p.broadcastImage(img)
						} else {
							seekAndRender(target, true)
						}
						p.broadcastState()
					}
					continue
				}

				var img []byte
				loopToStart := false
				stalled := false
				for i := 0; i < expectedFrames; i++ {
					p.mu.RLock()
					atEnd := p.state.CurrentFrame >= p.state.TotalFrames-1
					p.mu.RUnlock()
					if atEnd {
						loopToStart = true
						break
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

func (p *Player) restartFFmpeg(frame int) {
	if p.cmd != nil {
		p.cmd.Process.Kill()
		p.cmd.Wait()
		if p.stdoutReader != nil {
			p.stdoutReader.Close()
		}
	}

drain:
	for {
		select {
		case <-p.frameChan:
		default:
			break drain
		}
	}

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
		return
	}

	go p.parseJPEGsAndCache(stdout, frame, p.frameChan)
}

func getMetadata(file string) (float64, float64, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate,duration",
		"-of", "csv=p=0",
		file,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return 0, 0, fmt.Errorf("unexpected ffprobe output: %s", string(out))
	}

	fields := strings.Split(lines[0], ",")
	if len(fields) < 1 {
		return 0, 0, fmt.Errorf("unexpected fields: %s", lines[0])
	}

	fpsStr := strings.Split(fields[0], "/")
	if len(fpsStr) != 2 {
		return 0, 0, fmt.Errorf("invalid fps format: %s", fields[0])
	}
	num, _ := strconv.ParseFloat(fpsStr[0], 64)
	den, _ := strconv.ParseFloat(fpsStr[1], 64)
	fps := num / den

	var duration float64
	if len(fields) > 1 {
		duration, _ = strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
	}

	if duration == 0 {
		cmd = exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file)
		out, err = cmd.Output()
		if err == nil {
			duration, _ = strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
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
		select {
		case ch <- img:
		default:
		}
	}
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
