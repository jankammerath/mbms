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
	"regexp"
	"strings"
	"sync"
	"time"
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

func writeSMIL(w http.ResponseWriter, channel Channel, baseURL string) {
	fmt.Fprintf(w, `<!DOCTYPE smil PUBLIC "-//W3C//DTD SMIL 1.0//EN" "http://www.w3.org/TR/REC-smil/SMIL10.dtd">
<smil>
<body>
<video src="%s/stream/%s"/>
</body>
</smil>`, baseURL, channel.Slug)
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
	channel, found := findChannelBySlug(slug)
	if !found {
		http.NotFound(w, r)
		return
	}

	cmd := exec.Command("ffmpeg", "-i", channel.URL,
		"-c:v", "svq1", "-c:a", "adpcm_ima_qt",
		"-s", "240x180", "-b:v", "75k",
		"-r", "15", "-g", "15",
		"-ar", "22050", "-ac", "1",
		"-f", "mov",
		"-vtag", "SVQ1",
		"-")

	cmd.Stdout = w
	w.Header().Set("Content-Type", "video/quicktime")
	w.Header().Set("Content-Disposition", "inline; filename=stream.mov")

	if err := cmd.Run(); err != nil {
		log.Printf("Transcoding error: %v", err)
		http.Error(w, "Transcoding failed", http.StatusInternalServerError)
		return
	}
}

func smilHandler(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/smil/")
	slug = strings.TrimSuffix(slug, ".smi")
	channel, found := findChannelBySlug(slug)
	if !found {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/smil")
	writeSMIL(w, *channel, "http://"+r.Host)
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
	http.HandleFunc("/smil/", smilHandler)
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
<td><a href="/smil/{{$channel.Slug}}.smi">Watch in QuickTime</a></td>
</tr>
{{end}}
</table>
</body>
</html>`
