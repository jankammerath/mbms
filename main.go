package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
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
	MMSPort            = 1755     // Standard MMS port
)

// MMS protocol command types - expanded list based on MS-MMS protocol
const (
	MMS_CONNECT         = 0x00000001
	MMS_CONNECT_RESP    = 0x00000002
	MMS_PROTOCOL_SELECT = 0x00000003
	MMS_START_PLAY      = 0x00000007
	MMS_START_PLAY_RESP = 0x00000008
	MMS_DATA_PACKET     = 0x00000020
	MMS_END_OF_STREAM   = 0x00000021
	// Extended MMS commands
	MMS_HEADER_START = 0xB0000000
	MMS_HEADER_END   = 0xB1000000
	// Common received commands from WMP
	MMS_COMMAND_0FB00000 = 0x0FB00000
	MMS_COMMAND_0F000000 = 0x0F000000
	MMS_COMMAND_1F000000 = 0x1F000000
	MMS_COMMAND_B0000000 = 0xB0000000
	MMS_COMMAND_B1000000 = 0xB1000000
	// Additional commands seen in WMPMac logs
	MMS_COMMAND_3F000001 = 0x3F000001
	MMS_COMMAND_4B000001 = 0x4B000001
	MMS_COMMAND_B3000001 = 0xB3000001
	MMS_COMMAND_34000001 = 0x34000001
	MMS_COMMAND_23000001 = 0x23000001
	MMS_COMMAND_B2000001 = 0xB2000001
)

// MMSHeader represents an MMS protocol header
type MMSHeader struct {
	CommandID   uint32
	Reserved1   uint32
	Reserved2   uint32
	MessageLen  uint32
	SequenceNum uint32
	TimeoutVal  uint32
	Reserved3   uint32
	Reserved4   uint32
	Reserved5   uint32
	MessageLen2 uint32
}

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

func startMMSServer() {
	log.Printf("Starting MMS server on port %d", MMSPort)

	// Listen on the MMS port
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", MMSPort))
	if err != nil {
		log.Fatalf("Failed to start MMS server: %v", err)
	}

	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Error accepting connection: %v", err)
			continue
		}

		// Handle each MMS connection in a goroutine
		go handleMMSConnection(conn)
	}
}

func handleMMSConnection(conn net.Conn) {
	defer conn.Close()

	remoteAddr := conn.RemoteAddr().String()
	log.Printf("New MMS connection from %s", remoteAddr)

	// Buffer for reading MMS headers
	buf := make([]byte, 4096)

	// Read the initial MMS command
	n, err := conn.Read(buf)
	if err != nil {
		log.Printf("Error reading from connection %s: %v", remoteAddr, err)
		return
	}

	// Dump the raw received data in hex for debugging
	log.Printf("Received %d bytes from %s", n, remoteAddr)
	log.Printf("Raw data: %s", hex.Dump(buf[:n]))

	// Check if we have enough data for an MMS header
	if n < 40 {
		log.Printf("Received too short message from %s (%d bytes)", remoteAddr, n)
		return
	}

	var header MMSHeader
	headerBuf := bytes.NewBuffer(buf[:40])
	err = binary.Read(headerBuf, binary.LittleEndian, &header)
	if err != nil {
		log.Printf("Error parsing MMS header from %s: %v", remoteAddr, err)
		return
	}

	log.Printf("MMS command received from %s: CommandID=0x%08X", remoteAddr, header.CommandID)

	// Handle any command ID - respond with a generic MMS response
	handleGenericMMSCommand(conn, header, buf[40:n])
}

func handleGenericMMSCommand(conn net.Conn, header MMSHeader, data []byte) {
	remoteAddr := conn.RemoteAddr().String()
	cmdID := header.CommandID

	// Try to extract URL if present in the data
	url := ""
	for i := 0; i < len(data); i++ {
		if data[i] >= 32 && data[i] <= 127 {
			url += string(data[i])
		}
	}
	log.Printf("Possible URL or data content: %s", url)

	// Extract any potential channel from the URL or data
	var channel *Channel
	var found bool

	// Try to find the channel by parsing the URL
	for _, ch := range channels {
		if strings.Contains(url, ch.Slug) {
			channel = &ch
			found = true
			break
		}
	}

	// If no channel found, pick the first one as a fallback
	if !found && len(channels) > 0 {
		ch := channels[0]
		channel = &ch
		found = true
	}

	// Create a response header based on the command ID
	var respCmdID uint32
	switch cmdID {
	case MMS_CONNECT, MMS_COMMAND_0FB00000, MMS_COMMAND_0F000000:
		respCmdID = MMS_CONNECT_RESP
	case MMS_START_PLAY, MMS_COMMAND_1F000000:
		respCmdID = MMS_START_PLAY_RESP
	case MMS_COMMAND_3F000001, MMS_COMMAND_4B000001, MMS_COMMAND_B3000001,
		MMS_COMMAND_34000001, MMS_COMMAND_23000001, MMS_COMMAND_B2000001:
		// Handle the WMPMac specific commands - these appear to be connect commands
		respCmdID = MMS_CONNECT_RESP
	default:
		// For any other command, just acknowledge with connect response
		respCmdID = MMS_CONNECT_RESP
	}

	// Create a more complete MMS header response
	resp := MMSHeader{
		CommandID:   respCmdID,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  36,
		SequenceNum: header.SequenceNum + 1,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 36,
	}

	// Write response header
	respBuf := new(bytes.Buffer)
	if err := binary.Write(respBuf, binary.LittleEndian, resp); err != nil {
		log.Printf("Error creating MMS response: %v", err)
		return
	}

	if _, err := conn.Write(respBuf.Bytes()); err != nil {
		log.Printf("Error sending MMS response: %v", err)
		return
	}

	log.Printf("Sent MMS response 0x%08X to %s", respCmdID, remoteAddr)

	if found {
		streamToMMSClient(conn, channel)
	} else {
		log.Printf("No channel found to stream for %s", remoteAddr)
	}
}

func streamToMMSClient(conn net.Conn, channel *Channel) {
	log.Printf("Starting MMS stream for channel %s to %s", channel.Name, conn.RemoteAddr())

	// Create a temporary file with .asf extension
	tempDir := os.TempDir()
	tmpName := filepath.Join(tempDir, fmt.Sprintf("mms-stream-%s-%d.asf", channel.Slug, time.Now().UnixNano()))
	log.Printf("Using temporary file: %s", tmpName)

	// Send initial MMS headers to client
	sendMMSProtocolSelection(conn)

	// First, transcode to the temporary ASF file with improved options for WMP compatibility
	cmd := exec.Command("ffmpeg",
		"-v", "info", // More verbose logging to diagnose issues
		"-ignore_unknown", "1",
		"-i", channel.URL,
		"-sn", // Skip subtitles
		"-dn", // Skip data streams
		"-max_muxing_queue_size", "1024",
		"-map", "0:v:0?", // Map only first video stream if available
		"-map", "0:a:0?", // Map only first audio stream if available
		"-c:v", "wmv1",
		"-b:v", "300k",
		"-r", "25",
		"-g", "250",
		"-bf", "0",
		"-c:a", "wmav1",
		"-b:a", "64k",
		"-ar", "44100",
		"-ac", "2",
		"-vf", "scale=320:240",
		"-f", "asf", // Format for Windows Media
		"-asf_stream_properties", "parse_all=true",
		"-y",    // Overwrite output file
		tmpName) // Output file

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	log.Printf("Starting FFmpeg transcode to temporary file")
	if err := cmd.Start(); err != nil {
		log.Printf("Error starting FFmpeg: %v", err)
		return
	}

	// Start a goroutine to wait for FFmpeg to produce some output
	outputReady := make(chan bool)
	go func() {
		// Check every 200ms if the file exists and has some content
		for i := 0; i < 50; i++ { // Try for 10 seconds (50 * 200ms)
			time.Sleep(200 * time.Millisecond)
			info, err := os.Stat(tmpName)
			if err == nil && info.Size() > 0 {
				log.Printf("FFmpeg output file created with size: %d bytes", info.Size())
				outputReady <- true
				return
			}
		}
		// If we get here, FFmpeg failed to create/write to the output file
		log.Printf("FFmpeg failed to create output within timeout period")
		outputReady <- false
	}()

	// Wait for output to be ready or timeout
	var fileReady bool
	select {
	case fileReady = <-outputReady:
		if !fileReady {
			log.Printf("Timed out waiting for FFmpeg to produce output")
			cmd.Process.Kill()
			return
		}
	case <-time.After(15 * time.Second): // Longer timeout
		log.Printf("Timed out waiting for FFmpeg output channel")
		cmd.Process.Kill()
		return
	}

	// Let FFmpeg finish creating the header
	time.Sleep(1 * time.Second)

	// If we get here, the file was created successfully
	cmd.Process.Kill()
	cmd.Wait()

	// Check if the file actually has content
	fileInfo, err := os.Stat(tmpName)
	if err != nil || fileInfo.Size() == 0 {
		log.Printf("Output file is empty or cannot be accessed: %v", err)
		return
	}
	log.Printf("Output file size: %d bytes", fileInfo.Size())

	// Send MMS file header
	sendMMSHeaderStart(conn)

	// Open the file and stream it to the client
	file, err := os.Open(tmpName)
	if err != nil {
		log.Printf("Error opening temporary file: %v", err)
		return
	}
	defer file.Close()
	defer os.Remove(tmpName)

	// Send ASF header packets first
	headerBuf := make([]byte, 8192) // Larger buffer for ASF header
	n, err := file.Read(headerBuf)
	if err != nil {
		log.Printf("Error reading ASF header: %v", err)
		return
	}

	// Send the ASF header as an MMS_DATA_PACKET
	sendMMSDataPacket(conn, headerBuf[:n], 0)

	// Send MMS header end
	sendMMSHeaderEnd(conn)

	// Now send the actual data packets
	log.Printf("Starting to stream file content to client")
	packetSize := 4096
	buffer := make([]byte, packetSize)
	packetNumber := uint32(1) // Start packet numbering after header packet

	for {
		n, err := file.Read(buffer)
		if err == io.EOF {
			log.Printf("End of file reached")
			break
		}
		if err != nil {
			log.Printf("Error reading from temporary file: %v", err)
			break
		}

		// Send as MMS data packet
		sendMMSDataPacket(conn, buffer[:n], packetNumber)
		packetNumber++

		// Small delay between packets to prevent overwhelming the client
		time.Sleep(20 * time.Millisecond)
	}

	// Send end of stream marker
	sendMMSEndOfStream(conn)

	log.Printf("MMS stream ended for %s to %s", channel.Name, conn.RemoteAddr())
}

// sendMMSProtocolSelection sends the protocol selection command to the client
func sendMMSProtocolSelection(conn net.Conn) {
	header := MMSHeader{
		CommandID:   MMS_PROTOCOL_SELECT,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  36,
		SequenceNum: 1,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 36,
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		log.Printf("Error creating MMS protocol selection header: %v", err)
		return
	}

	if _, err := conn.Write(buf.Bytes()); err != nil {
		log.Printf("Error sending MMS protocol selection header: %v", err)
		return
	}

	log.Printf("Sent MMS protocol selection header")
}

// sendMMSHeaderStart informs the client that the header data is coming
func sendMMSHeaderStart(conn net.Conn) {
	header := MMSHeader{
		CommandID:   MMS_HEADER_START,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  36,
		SequenceNum: 2,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 36,
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		log.Printf("Error creating MMS header start: %v", err)
		return
	}

	if _, err := conn.Write(buf.Bytes()); err != nil {
		log.Printf("Error sending MMS header start: %v", err)
		return
	}

	log.Printf("Sent MMS header start")
}

// sendMMSHeaderEnd signals the end of header data
func sendMMSHeaderEnd(conn net.Conn) {
	header := MMSHeader{
		CommandID:   MMS_HEADER_END,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  36,
		SequenceNum: 3,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 36,
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		log.Printf("Error creating MMS header end: %v", err)
		return
	}

	if _, err := conn.Write(buf.Bytes()); err != nil {
		log.Printf("Error sending MMS header end: %v", err)
		return
	}

	log.Printf("Sent MMS header end")
}

// sendMMSDataPacket sends actual media data to the client
func sendMMSDataPacket(conn net.Conn, data []byte, packetNumber uint32) {
	header := MMSHeader{
		CommandID:   MMS_DATA_PACKET,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  uint32(len(data) + 36),
		SequenceNum: packetNumber,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: uint32(len(data) + 36),
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		log.Printf("Error creating MMS data packet header: %v", err)
		return
	}

	if _, err := conn.Write(buf.Bytes()); err != nil {
		log.Printf("Error sending MMS data packet header: %v", err)
		return
	}

	if _, err := conn.Write(data); err != nil {
		log.Printf("Error sending MMS data packet data: %v", err)
		return
	}
}

// sendMMSEndOfStream signals the end of the stream
func sendMMSEndOfStream(conn net.Conn) {
	header := MMSHeader{
		CommandID:   MMS_END_OF_STREAM,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  36,
		SequenceNum: 4,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 36,
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, header); err != nil {
		log.Printf("Error creating MMS end of stream header: %v", err)
		return
	}

	if _, err := conn.Write(buf.Bytes()); err != nil {
		log.Printf("Error sending MMS end of stream header: %v", err)
		return
	}

	log.Printf("Sent MMS end of stream header")
}

func writeASX(w http.ResponseWriter, channel Channel, host string) {
	// Extract host without port if necessary
	hostOnly := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	}

	// Create a Windows Media Player compatible ASX file with MMS protocol
	fmt.Fprintf(w, `<ASX VERSION="3.0">
<ENTRY>
<TITLE>%s</TITLE>
<REF HREF="%s%s:%d/%s.wmv"/>
</ENTRY>
</ASX>`, channel.Name, MMSProtocol, hostOnly, MMSPort, channel.Slug)
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

	// Special headers for Windows Media Player 7
	w.Header().Set("Content-Type", "application/x-mms-framed")
	w.Header().Set("Server", "WMServer/9.0")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Server-Name", "Windows Media Server")
	w.Header().Set("Content-Length", "") // Force chunked transfer
	w.(http.Flusher).Flush()

	log.Printf("Starting FFmpeg transcoding for channel %s using MMS protocol", channel.Name)

	// Use FFmpeg with settings optimized specifically for Windows Media Player 7
	cmd := exec.Command("ffmpeg",
		"-v", "info",
		"-re",
		"-i", channel.URL,
		"-c:v", "wmv1", // Use WMV1 codec for better WMP7 compatibility
		"-b:v", "300k",
		"-r", "25", // 25 fps
		"-g", "250", // Keyframe every 10 seconds
		"-bf", "0", // No B-frames
		"-qmin", "2", // Minimum quantizer
		"-qmax", "31", // Maximum quantizer
		"-c:a", "wmav1", // WMA version 1 for better compatibility
		"-b:a", "64k", // Lower audio bitrate for stability
		"-ar", "44100", // Standard audio rate
		"-ac", "2", // Stereo
		"-vf", "scale=320:240", // Smaller resolution for better performance
		"-f", "asf",
		"-packetsize", "2048", // Smaller packet size
		"-movflags", "faststart",
		"-fflags", "+genpts+ignidx",
		"-sn",           // Skip subtitles
		"-map", "0:v:0", // Map first video stream
		"-map", "0:a:0", // Map first audio stream
		"-")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = w

	if err := cmd.Run(); err != nil {
		// Ignore common disconnection errors
		if !strings.Contains(err.Error(), "broken pipe") &&
			!strings.Contains(err.Error(), "exit status 224") &&
			!strings.Contains(err.Error(), "connection reset") {
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

	// Start MMS server in a goroutine
	go startMMSServer()

	// Keep HTTP server for the web interface
	http.HandleFunc("/stream/", streamHandler) // Keep for backward compatibility
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

	log.Printf("Web server starting on http://localhost:8080")
	log.Printf("MMS server running on port %d", MMSPort)
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
