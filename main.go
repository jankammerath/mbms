package main

import (
	"bufio"
	"bytes"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	VideoFileExtension = ".asx"
	MMSProtocol        = "mms://" // MMS protocol prefix
)

type Channel struct {
	Name string
	URL  string
	Slug string
}

type TemplateData struct {
	Channels []Channel
	Time     string
}

var (
	channels     []Channel
	screenshotMu sync.Mutex
)

func createSlug(name string) string {
	reg := regexp.MustCompile("[^a-zA-Z0-9]+")
	return strings.ToLower(reg.ReplaceAllString(name, "-"))
}

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
			currentChannel.Slug = createSlug(currentChannel.Name)
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
		for _, channel := range channels {
			outputPath := filepath.Join("screenshots", channel.Slug+".jpg")
			cmd := exec.Command("ffmpeg", "-y", "-i", channel.URL,
				"-vframes", "1", "-s", "240x180", "-q:v", "2", outputPath)
			if err := cmd.Run(); err != nil {
				log.Printf("Error capturing screenshot for %s: %v", channel.Name, err)
				continue
			}
		}
		screenshotMu.Unlock()
		time.Sleep(5 * time.Minute)
	}
}

func writeASX(w http.ResponseWriter, channel Channel, host string) {
	// Create a Windows Media Player compatible ASX file with MMS protocol
	fmt.Fprintf(w, `<ASX VERSION="3.0">
<ENTRY>
<TITLE>%s</TITLE>
<REF HREF="%s%s/stream/%s.wmv"/>
</ENTRY>
</ASX>`, channel.Name, MMSProtocol, host, channel.Slug)
}

func findChannelBySlug(slug string) (*Channel, bool) {
	for _, ch := range channels {
		if ch.Slug == slug {
			return &ch, true
		}
	}
	return nil, false
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/stream/")
	slug = strings.TrimSuffix(slug, ".wmv")
	channel, found := findChannelBySlug(slug)
	if !found {
		http.NotFound(w, r)
		return
	}

	log.Printf("Stream request from %s for channel %s (%s)", r.RemoteAddr, channel.Name, r.URL.Path)

	// Log the entire request data for debugging
	log.Printf("--- Begin Request Details ---")
	log.Printf("Method: %s", r.Method)
	log.Printf("URL: %s", r.URL.String())
	log.Printf("Protocol: %s", r.Proto)
	log.Printf("Host: %s", r.Host)
	log.Printf("Remote Address: %s", r.RemoteAddr)

	// Log all request headers
	log.Println("Headers:")
	for name, values := range r.Header {
		for _, value := range values {
			log.Printf("  %s: %s", name, value)
		}
	}

	// Log request cookies
	if len(r.Cookies()) > 0 {
		log.Println("Cookies:")
		for _, cookie := range r.Cookies() {
			log.Printf("  %s: %s", cookie.Name, cookie.Value)
		}
	}

	// Log any URL parameters
	if r.URL.RawQuery != "" {
		log.Printf("Query Parameters: %s", r.URL.RawQuery)
	}

	// Log form data if present
	r.ParseForm()
	if len(r.Form) > 0 {
		log.Println("Form Data:")
		for key, values := range r.Form {
			for _, value := range values {
				log.Printf("  %s: %s", key, value)
			}
		}
	}
	log.Printf("--- End Request Details ---")

	// Set headers for Windows Media Player compatibility with MMS protocol
	w.Header().Set("Content-Type", "video/x-ms-wmv")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Cache-Control", "no-cache")
	w.(http.Flusher).Flush()

	log.Printf("Starting FFmpeg transcoding for channel %s", channel.Name)

	// Use FFmpeg with settings optimized for Windows Media Player 7 with MMS protocol
	cmd := exec.Command("ffmpeg",
		"-i", channel.URL,
		"-c:v", "msmpeg4v3",
		"-b:v", "300k",
		"-r", "25", // Reduce framerate to 25fps
		"-g", "250", // Keyframe every 10 seconds at 25fps
		"-bf", "0", // No B-frames for better compatibility
		"-c:a", "wmav2",
		"-b:a", "128k",
		"-ar", "44100", // 44.1kHz audio sample rate
		"-ac", "2", // Stereo audio
		"-vf", "scale=640:360",
		"-f", "asf", // ASF container for WMV/WMA
		"-packetsize", "3200",
		"-map", "0:v:0", // Map the first video stream
		"-map", "0:a:0", // Map the first audio stream
		"-sn",                        // Skip subtitles
		"-max_interleave_delta", "0", // Reduce interleave delta for more resilience
		"-flush_packets", "1", // Flush packets immediately
		"-muxdelay", "0", // No mux delay
		"-")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = w

	if err := cmd.Run(); err != nil {
		// Ignore broken pipe errors (client disconnected) to avoid flooding logs
		if !strings.Contains(err.Error(), "broken pipe") && !strings.Contains(err.Error(), "exit status 224") {
			log.Printf("Transcoding error for %s: %v\nFFmpeg output:\n%s", channel.Name, err, stderr.String())
		} else {
			log.Printf("Client disconnected from %s stream", channel.Name)
		}
		return
	}

	log.Printf("Stream ended for channel %s", channel.Name)
}

func asxHandler(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/asx/")
	slug = strings.TrimSuffix(slug, ".asx")
	channel, found := findChannelBySlug(slug)
	if !found {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "video/x-ms-asf")
	writeASX(w, *channel, r.Host)
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

	http.HandleFunc("/stream/", streamHandler)
	http.HandleFunc("/asx/", asxHandler)
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
<meta http-equiv="Content-Type" content="text/html; charset=iso-8859-1">
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
{{range $channel := .Channels}}
<tr>
<td>{{$channel.Name}}</td>
<td><img src="/screenshots/{{$channel.Slug}}.jpg" width="160" height="90" alt="{{$channel.Name}} preview"></td>
<td><a href="/asx/{{$channel.Slug}}.asx">Watch in Windows Media Player</a></td>
</tr>
{{end}}
</table>
</body>
</html>`
