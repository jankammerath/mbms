package main

import (
	"bufio"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Channel struct {
	Name string
	URL  string
}

type TemplateData struct {
	Channels []Channel
	Time     string
}

var (
	channels     []Channel
	screenshotMu sync.Mutex
)

func parseM3U(filename string) ([]Channel, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var channels []Channel
	scanner := bufio.NewScanner(file)
	var currentChannel Channel

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#EXTINF:-1,") {
			currentChannel.Name = strings.TrimPrefix(line, "#EXTINF:-1,")
		} else if !strings.HasPrefix(line, "#") && line != "" {
			currentChannel.URL = line
			channels = append(channels, currentChannel)
			currentChannel = Channel{}
		}
	}
	return channels, scanner.Err()
}

func captureScreenshots() {
	for {
		screenshotMu.Lock()
		for i, channel := range channels {
			outputPath := filepath.Join("screenshots", fmt.Sprintf("channel_%d.jpg", i))
			cmd := exec.Command("ffmpeg", "-y", "-i", channel.URL,
				"-vframes", "1", "-s", "320x240", "-q:v", "2", outputPath)
			if err := cmd.Run(); err != nil {
				log.Printf("Error capturing screenshot for %s: %v", channel.Name, err)
				continue
			}
		}
		screenshotMu.Unlock()
		time.Sleep(5 * time.Minute)
	}
}

func transcodeHandler(w http.ResponseWriter, r *http.Request) {
	channelIndex := r.URL.Query().Get("channel")
	if channelIndex == "" {
		http.Error(w, "Channel parameter required", http.StatusBadRequest)
		return
	}

	idx, err := strconv.Atoi(channelIndex)
	if err != nil || idx < 0 || idx >= len(channels) {
		http.Error(w, "Invalid channel index", http.StatusBadRequest)
		return
	}

	ch := channels[idx]
	cmd := exec.Command("ffmpeg", "-i", ch.URL,
		"-c:v", "wmv2", "-c:a", "wmav2",
		"-s", "320x240", "-b:v", "150k", "-b:a", "32k",
		"-ar", "22050", "-ac", "2",
		"-f", "asf", "-")

	cmd.Stdout = w
	w.Header().Set("Content-Type", "video/x-ms-asf")
	w.Header().Set("Content-Disposition", "inline; filename=stream.asf")

	if err := cmd.Run(); err != nil {
		log.Printf("Transcoding error: %v", err)
		http.Error(w, "Transcoding failed", http.StatusInternalServerError)
		return
	}
}

func checkFFmpeg() error {
	cmd := exec.Command("ffmpeg", "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg not found: %v", err)
	}
	return nil
}

func main() {
	if err := checkFFmpeg(); err != nil {
		log.Fatal(err)
	}

	var err error
	channels, err = parseM3U("channels.m3u")
	if err != nil {
		log.Fatal(err)
	}

	if err := os.MkdirAll("screenshots", 0755); err != nil {
		log.Fatal(err)
	}

	go captureScreenshots()

	http.HandleFunc("/stream", transcodeHandler)
	http.Handle("/screenshots/", http.StripPrefix("/screenshots/", http.FileServer(http.Dir("screenshots"))))

	tmpl := template.Must(template.New("index").Parse(indexHTML))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data := TemplateData{
			Channels: channels,
			Time:     time.Now().Format("2006-01-02 15:04:05"),
		}
		tmpl.Execute(w, data)
	})

	log.Println("Server starting on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

const indexHTML = `<!DOCTYPE html PUBLIC "-//W3C//DTD HTML 3.2//EN">
<html>
<head>
<title>TV Channel Viewer</title>
<meta http-equiv="refresh" content="300">
</head>
<body bgcolor="#FFFFFF">
<h1>TV Channel List</h1>
<p>Last updated: {{.Time}}</p>
<table border="1" cellpadding="5" cellspacing="0">
<tr>
<th>Channel</th>
<th>Preview</th>
<th>Action</th>
</tr>
{{range $i, $channel := .Channels}}
<tr>
<td>{{$channel.Name}}</td>
<td><img src="/screenshots/channel_{{$i}}.jpg" width="160" height="90" alt="{{$channel.Name}} preview"></td>
<td><a href="/stream?channel={{$i}}">Watch in WMP</a></td>
</tr>
{{end}}
</table>
</body>
</html>`
