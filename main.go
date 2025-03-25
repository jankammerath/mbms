package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/gosimple/slug"
)

const M3U_FILE = "channels.m3u"
const VIDEO_FILE_EXT = ".mov"

func main() {
	channels, err := parseM3U(M3U_FILE)
	if err != nil {
		log.Fatalf("Failed to parse M3U file: %v", err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var channelListHTML strings.Builder
		channelListHTML.WriteString("<h1>Channels</h1><ul>")
		for _, channel := range channels {
			encodedName := slug.Make(channel.Name) + VIDEO_FILE_EXT // URL-encode the channel name
			channelListHTML.WriteString(fmt.Sprintf("<li><a href=\"/%s\">%s</a></li>", encodedName, channel.Name))
		}
		channelListHTML.WriteString("</ul>")
		fmt.Fprint(w, channelListHTML.String())
	})

	for _, channel := range channels {
		channelURL := "/" + slug.Make(channel.Name) + VIDEO_FILE_EXT // URL-encode the channel name
		http.HandleFunc(channelURL, func(w http.ResponseWriter, r *http.Request) {
			// Construct FFmpeg command Quicktime
			ffmpegCmd := []string{
				"ffmpeg",
				"-i", channel.URL,
				"-c:v", "svq1",
				"-s", "640x360",
				"-aspect", "16:9",
				"-r", "25",
				"-b:v", "320k",
				"-c:a", "aac",
				"-ar", "44100",
				"-ac", "2",
				"-b:a", "128k",
				"-f", "mov",
				"-", // Output to stdout
			}

			cmd := exec.Command(ffmpegCmd[0], ffmpegCmd[1:]...)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				log.Printf("Error creating stdout pipe: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				log.Printf("Error creating stderr pipe: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			if err := cmd.Start(); err != nil {
				log.Printf("Error starting FFmpeg: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}

			// Log FFmpeg's stderr
			go func() {
				scanner := bufio.NewScanner(stderr)
				for scanner.Scan() {
					log.Printf("FFmpeg stderr: %s", scanner.Text())
				}
			}()

			// Set appropriate headers for streaming WMV
			w.Header().Set("Content-Type", "video/quicktime") // or "video/wmv"
			w.Header().Set("Transfer-Encoding", "chunked")    // Enable chunked transfer

			// Stream the output to the client
			buffer := make([]byte, 4096)
			for {
				bytesRead, err := stdout.Read(buffer)
				if err != nil {
					if err != io.EOF {
						log.Printf("Error reading from FFmpeg: %v", err)
					}
					break
				}
				_, err = w.Write(buffer[:bytesRead])
				if err != nil {
					log.Printf("Error writing to client: %v", err)
					break // Client disconnected
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush() // Flush the buffer to the client
				}
			}

			err = cmd.Wait()
			if err != nil {
				log.Printf("FFmpeg finished with error: %v", err)
			} else {
				log.Println("FFmpeg finished successfully")
			}
		})
	}

	fmt.Println("Server listening on port 8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

type Channel struct {
	Name string
	URL  string
}

func parseM3U(filename string) ([]Channel, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	var channels []Channel
	var currentChannel Channel

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])

		if strings.HasPrefix(line, "#EXTINF:") {
			// Extract channel name
			parts := strings.SplitN(line, ",", 2)
			if len(parts) > 1 {
				currentChannel.Name = strings.TrimSpace(parts[1])
			}
		} else if !strings.HasPrefix(line, "#") && line != "" {
			// Assume this is the URL
			currentChannel.URL = strings.TrimSpace(line)
			channels = append(channels, currentChannel)
			currentChannel = Channel{} // Reset for the next channel
		}
	}

	return channels, nil
}
