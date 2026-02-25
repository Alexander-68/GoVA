package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
)

var p *Player

func main() {
	var fileFlag = flag.String("file", "", "Path to video file to load initially")
	var portFlag = flag.Int("port", 8080, "Port to run the web server on")
	flag.Parse()

	p = NewPlayer()

	if *fileFlag != "" {
		err := p.Load(*fileFlag)
		if err != nil {
			log.Printf("Failed to load initial file: %v", err)
		}
	}

	http.Handle("/", http.FileServer(http.Dir("./static")))

	http.HandleFunc("/api/load", handleLoad)
	http.HandleFunc("/api/play", handlePlay)
	http.HandleFunc("/api/play-backward", handlePlayBackward)
	http.HandleFunc("/api/pause", handlePause)
	http.HandleFunc("/api/step", handleStep)
	http.HandleFunc("/api/seek", handleSeek)
	http.HandleFunc("/api/fps", handleFPS)

	http.HandleFunc("/stream", handleStream)
	http.HandleFunc("/state", handleState)

	addr := fmt.Sprintf(":%d", *portFlag)
	log.Printf("Starting GoVA server on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func handleLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.ParseForm()
	file := r.FormValue("file")
	err := p.Load(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func handlePlay(w http.ResponseWriter, r *http.Request) {
	p.Play()
	w.WriteHeader(http.StatusOK)
}

func handlePlayBackward(w http.ResponseWriter, r *http.Request) {
	p.PlayBackward()
	w.WriteHeader(http.StatusOK)
}

func handlePause(w http.ResponseWriter, r *http.Request) {
	p.Pause()
	w.WriteHeader(http.StatusOK)
}

func handleStep(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	frames, err := strconv.Atoi(r.FormValue("frames"))
	if err == nil {
		p.Step(frames)
	}
	w.WriteHeader(http.StatusOK)
}

func handleSeek(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	frame, err := strconv.Atoi(r.FormValue("frame"))
	if err == nil {
		p.Seek(frame)
	}
	w.WriteHeader(http.StatusOK)
}

func handleFPS(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	fps, err := strconv.ParseFloat(r.FormValue("fps"), 64)
	if err == nil {
		p.SetFPS(fps)
	}
	w.WriteHeader(http.StatusOK)
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")

	ch := p.SubscribeStream()
	defer p.UnsubscribeStream(ch)

	// Send initial/current frame if available
	img := p.GetCurrentImage()
	if img != nil {
		writeFrame(w, img)
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case img := <-ch:
			if err := writeFrame(w, img); err != nil {
				return
			}
		}
	}
}

func writeFrame(w http.ResponseWriter, img []byte) error {
	_, err := w.Write([]byte("--frame\r\nContent-Type: image/jpeg\r\n\r\n"))
	if err != nil {
		return err
	}
	_, err = w.Write(img)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte("\r\n"))
	if err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func handleState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := p.SubscribeState()
	defer p.UnsubscribeState(ch)

	// Send current state immediately
	sendState(w, p.GetState())

	for {
		select {
		case <-r.Context().Done():
			return
		case s := <-ch:
			if err := sendState(w, s); err != nil {
				return
			}
		}
	}
}

func sendState(w http.ResponseWriter, s State) error {
	b, _ := json.Marshal(s)
	_, err := fmt.Fprintf(w, "data: %s\n\n", string(b))
	if err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}
