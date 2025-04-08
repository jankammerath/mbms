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

	// Standard MMS protocol message IDs from MS-MMS specification
	MMS_LinkViewerToMacConnect            = 0x00000001
	MMS_LinkMacToViewerReportConnectedEX  = 0x00000002
	MMS_LinkViewerToMacOpenFile           = 0x00000005
	MMS_LinkMacToViewerReportOpenFile     = 0x00000006
	MMS_LinkViewerToMacReadBlock          = 0x00000011
	MMS_LinkViewerToMacStartPlaying       = 0x00000007
	MMS_LinkMacToViewerReportStartPlaying = 0x00000008
	MMS_LinkViewerToMacStopPlaying        = 0x00000009
	MMS_LinkMacToViewerReportStopPlaying  = 0x0000000A
	MMS_LinkMacToViewerDataPacket         = 0x00000020
	MMS_LinkMacToViewerEndOfStream        = 0x00000021

	// Additional MMS commands for specific clients
	MMS_HEADER_START = 0xB0000000
	MMS_HEADER_END   = 0xB1000000

	// Common received commands from WMP
	MMS_COMMAND_0F000000 = 0x0F000000
	MMS_COMMAND_1F000000 = 0x1F000000

	// MMS protocol message types
	MMS_MESSAGE_TYPE_DATA    = 0x00000000
	MMS_MESSAGE_TYPE_END     = 0x00000001
	MMS_MESSAGE_TYPE_ACK     = 0x00000002
	MMS_MESSAGE_TYPE_REQUEST = 0x00000003
	MMS_MESSAGE_TYPE_REPORT  = 0x00000004
)

// Add more constants for the MMS protocol
const (
	// Additional compatibility constants
	MMS_CONNECT         = MMS_LinkViewerToMacConnect           // 0x00000001
	MMS_CONNECT_RESP    = MMS_LinkMacToViewerReportConnectedEX // 0x00000002
	MMS_PROTOCOL_SELECT = 0x00000003                           // Protocol selection command
	MMS_START_PLAY      = MMS_LinkViewerToMacStartPlaying      // 0x00000007
	MMS_DATA_PACKET     = MMS_LinkMacToViewerDataPacket        // 0x00000020
	MMS_END_OF_STREAM   = MMS_LinkMacToViewerEndOfStream       // 0x00000021
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

// MMSClientState tracks the state of an MMS client connection through the protocol stages
type MMSClientState struct {
	Channel     *Channel
	SeqNum      uint32
	PlayingFile bool
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

// Define the standard MS-MMS specific message structures
type MMSMessage struct {
	Header MMSHeader
	Body   []byte
}

// handleMMSConnection updated with proper MS-MMS protocol sequence
func handleMMSConnection(conn net.Conn) {
	defer conn.Close()

	remoteAddr := conn.RemoteAddr().String()
	log.Printf("New MMS connection from %s", remoteAddr)

	// Initialize client state
	clientState := &MMSClientState{
		SeqNum:      0,
		PlayingFile: false,
	}

	// Main protocol loop
	for {
		// Buffer for reading MMS headers
		buf := make([]byte, 4096)

		// Read the MMS command
		n, err := conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading from connection %s: %v", remoteAddr, err)
			} else {
				log.Printf("Client %s disconnected", remoteAddr)
			}
			return
		}

		// Check if we received any data
		if n == 0 {
			log.Printf("Received empty message from %s", remoteAddr)
			continue
		}

		// Dump the raw received data in hex for debugging (small samples only)
		if n < 100 {
			log.Printf("Received %d bytes from %s", n, remoteAddr)
			log.Printf("Raw data: %s", hex.Dump(buf[:n]))
		} else {
			log.Printf("Received %d bytes from %s (too large to dump)", n, remoteAddr)
		}

		// Check if we have enough data for an MMS header
		if n < 40 {
			log.Printf("Received too short message from %s (%d bytes)", remoteAddr, n)
			continue
		}

		var header MMSHeader
		headerBuf := bytes.NewBuffer(buf[:40])
		if err := binary.Read(headerBuf, binary.LittleEndian, &header); err != nil {
			log.Printf("Error parsing MMS header from %s: %v", remoteAddr, err)
			continue
		}

		log.Printf("MMS command received from %s: CommandID=0x%08X, SeqNum=%d",
			remoteAddr, header.CommandID, header.SequenceNum)

		// Update the client sequence number
		clientState.SeqNum = header.SequenceNum

		// Extract client info for better logging (only on the first command)
		if clientState.SeqNum == 0 {
			clientInfo := extractClientInfo(buf[40:n])
			if clientInfo != "" {
				log.Printf("Client info: %s", clientInfo)
			}
		}

		// Handle the command based on the MMS protocol state machine
		switch header.CommandID {
		case MMS_LinkViewerToMacConnect:
			// Client connection request - send connection response
			sendMMSConnectResponse(conn, header.SequenceNum)

		case MMS_LinkViewerToMacOpenFile:
			// Client requesting to open a file
			log.Printf("Client requesting to open file")

			// Parse the file path from the data
			path := extractURLFromData(buf[40:n])
			log.Printf("Requested file path: %s", path)

			// Find channel by URL/path
			channel := findChannelFromPath(path)
			if channel != nil {
				clientState.Channel = channel
				log.Printf("Found channel: %s", channel.Name)

				// Send file opened response
				sendMMSFileOpenedResponse(conn, header.SequenceNum)
			} else {
				log.Printf("No matching channel found for path: %s", path)
				// Default to first channel if available
				if len(channels) > 0 {
					ch := channels[0]
					clientState.Channel = &ch
					sendMMSFileOpenedResponse(conn, header.SequenceNum)
				} else {
					// No channels available
					return
				}
			}

		case MMS_LinkViewerToMacReadBlock:
			// Client requesting file header
			log.Printf("Client requesting to read file header block")
			// This usually comes after opening a file
			if clientState.Channel != nil && !clientState.PlayingFile {
				// Prepare the ASF header and send it
				sendPreloadedASFHeader(conn, clientState)
			}

		case MMS_LinkViewerToMacStartPlaying:
			// Client requesting to start playback
			log.Printf("Client requesting to start playback")

			if clientState.Channel != nil {
				// Send start playing response
				sendMMSStartPlayingResponse(conn, header.SequenceNum)

				// Start streaming the content
				clientState.PlayingFile = true
				go streamToMMSClient(conn, clientState.Channel)
				// Note: The goroutine will continue streaming until the connection closes
				return
			}

		case MMS_LinkViewerToMacStopPlaying:
			// Client requesting to stop playback
			log.Printf("Client requesting to stop playback")
			sendMMSStopPlayingResponse(conn, header.SequenceNum)
			clientState.PlayingFile = false

		default:
			// Handle other commands or malformed packets
			log.Printf("Unhandled command from %s: 0x%08X", remoteAddr, header.CommandID)

			// If no channel is selected yet, try to extract a channel from the data
			if clientState.Channel == nil && n > 40 {
				path := extractURLFromData(buf[40:n])
				if path != "" {
					channel := findChannelFromPath(path)
					if channel != nil {
						clientState.Channel = channel
						log.Printf("Found channel from data: %s", channel.Name)

						// For compatibility with older clients, proceed directly to streaming
						sendMMSConnectResponse(conn, header.SequenceNum)
						clientState.PlayingFile = true
						go streamToMMSClient(conn, clientState.Channel)
						return
					}
				}

				// If no channel found, use the first one (if available)
				if clientState.Channel == nil && len(channels) > 0 {
					ch := channels[0]
					clientState.Channel = &ch
					log.Printf("Using default channel: %s", ch.Name)

					sendMMSConnectResponse(conn, header.SequenceNum)
					clientState.PlayingFile = true
					go streamToMMSClient(conn, clientState.Channel)
					return
				}
			}
		}
	}
}

// Extract client information (User-Agent) from the connection data
func extractClientInfo(data []byte) string {
	// Extract printable ASCII characters that might represent user agent
	var info strings.Builder
	inUserAgent := false

	for i := 0; i < len(data); i++ {
		// Look for "NSPlayer" string which typically indicates user-agent info
		if i+7 < len(data) && string(data[i:i+8]) == "NSPlayer" {
			inUserAgent = true
		}

		if inUserAgent && data[i] >= 32 && data[i] <= 126 {
			info.WriteByte(data[i])
		}
	}

	return info.String()
}

// Extract a URL or file path from MMS data
func extractURLFromData(data []byte) string {
	// Extract printable ASCII characters
	var url strings.Builder
	for _, b := range data {
		if b >= 32 && b <= 126 {
			url.WriteByte(b)
		}
	}

	return url.String()
}

// Find a channel by path or URL
func findChannelFromPath(path string) *Channel {
	for _, ch := range channels {
		// Check if the path contains the channel slug
		if strings.Contains(strings.ToLower(path), ch.Slug) {
			return &ch
		}
	}
	return nil
}

// Handle a WMPMac client connection following MS-MMS protocol
func handleWMPMacConnection(conn net.Conn, header MMSHeader, data []byte) {
	remoteAddr := conn.RemoteAddr().String()
	cmdID := header.CommandID

	// Log the command ID type for better diagnostics
	var cmdType string
	if cmdID == MMS_LinkViewerToMacConnect {
		cmdType = "Connect Request"
	} else if cmdID == MMS_LinkViewerToMacStartPlaying {
		cmdType = "Start Play Request"
	} else if cmdID == MMS_COMMAND_0F000000 {
		cmdType = "Standard Connect"
	} else if cmdID == MMS_COMMAND_1F000000 {
		cmdType = "Start Play Request"
	} else {
		cmdType = fmt.Sprintf("Unknown Command Type (0x%08X)", cmdID)
	}

	log.Printf("Processing %s command from %s", cmdType, remoteAddr)

	// We've received a connection request - respond with connect response
	sendMMSConnectResponse(conn, header.SequenceNum)

	// For WMPMac in the current protocol stage, we can directly start sending
	// media packets after sending a protocol selection message

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

	if found {
		// For most complete MMS protocol compliance we should actually:
		// 1. Wait for LinkViewerToMacOpenFile
		// 2. Send LinkMacToViewerReportOpenFile
		// 3. Wait for LinkViewerToMacStartPlaying
		// 4. Send LinkMacToViewerReportStartPlaying

		// But for WMPMac 7.1 compatibility, we'll use the simplified approach
		// that just streams the content after the initial connection
		streamToMMSClient(conn, channel)
	} else {
		log.Printf("No channel found to stream for %s", remoteAddr)
	}
}

// sendMMSConnectResponse sends an MMS connection response message
func sendMMSConnectResponse(conn net.Conn, seqNum uint32) {
	// Clear response message that conforms to MMS protocol expectations
	response := MMSHeader{
		CommandID:   MMS_CONNECT_RESP, // 0x00000002
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  40,
		SequenceNum: seqNum + 1,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 40,
	}

	respBuf := new(bytes.Buffer)
	if err := binary.Write(respBuf, binary.LittleEndian, response); err != nil {
		log.Printf("Error creating MMS connect response: %v", err)
		return
	}

	// Add a zero protocol selection at the end (helps with certain clients)
	protocolSelection := uint32(0)
	if err := binary.Write(respBuf, binary.LittleEndian, protocolSelection); err != nil {
		log.Printf("Error adding protocol selection to connect response: %v", err)
		return
	}

	if _, err := conn.Write(respBuf.Bytes()); err != nil {
		log.Printf("Error sending MMS connect response: %v", err)
		return
	}

	log.Printf("Sent MMS connect response (0x00000002)")
}

func streamToMMSClient(conn net.Conn, channel *Channel) {
	log.Printf("Starting MMS stream for channel %s to %s", channel.Name, conn.RemoteAddr())

	// Create a temporary file with .asf extension
	tempDir := os.TempDir()
	tmpName := filepath.Join(tempDir, fmt.Sprintf("mms-stream-%s-%d.asf", channel.Slug, time.Now().UnixNano()))
	log.Printf("Using temporary file: %s", tmpName)

	// First, transcode to the temporary ASF file with improved options for WMP compatibility
	cmd := exec.Command("ffmpeg",
		"-v", "info", // More verbose logging to diagnose issues
		"-nostdin", // Don't expect stdin input to avoid hanging
		"-i", channel.URL,
		"-sn",                            // Skip subtitles
		"-dn",                            // Skip data streams
		"-max_muxing_queue_size", "1024", // Handle larger input buffers
		"-map", "0:v:0?", // Map only first video stream if available
		"-map", "0:a:0?", // Map only first audio stream if available
		"-c:v", "wmv1", // Use WMV1 codec for better WMP7 compatibility
		"-b:v", "300k", // Reasonable video bitrate
		"-r", "25", // 25 fps
		"-g", "250", // Keyframe every 10 seconds
		"-bf", "0", // No B-frames for better compatibility
		"-c:a", "wmav1", // WMA version 1 for better compatibility
		"-b:a", "64k", // Audio bitrate
		"-ar", "44100", // Standard audio rate
		"-ac", "2", // Stereo audio
		"-vf", "scale=320:240", // Smaller resolution for better streaming
		"-f", "asf", // Format for Windows Media
		"-asf_stream_properties", "parse_all=true", // Better ASF compatibility
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

	// Open the file and stream it to the client
	file, err := os.Open(tmpName)
	if err != nil {
		log.Printf("Error opening temporary file: %v", err)
		return
	}
	defer file.Close()
	defer os.Remove(tmpName)

	// First, send a protocol selection message (if not already sent)
	sendMMSProtocolSelection(conn)

	// Send MMS file header start notification
	sendMMSHeaderStart(conn)

	// Send ASF header packets first
	headerBuf := make([]byte, 8192) // Larger buffer for ASF header
	n, err := file.Read(headerBuf)
	if err != nil {
		log.Printf("Error reading ASF header: %v", err)
		return
	}

	// Send the ASF header as an MMS_DATA_PACKET using the LinkMacToViewerDataPacket command ID
	header := MMSHeader{
		CommandID:   MMS_LinkMacToViewerDataPacket, // Using standard MS-MMS command ID
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  uint32(n + 40), // Add header size
		SequenceNum: 0,              // Header is always packet 0
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: uint32(n + 40),
	}

	// Send header with proper MS-MMS data packet format
	packetHeaderBuf := new(bytes.Buffer)
	if err := binary.Write(packetHeaderBuf, binary.LittleEndian, header); err != nil {
		log.Printf("Error creating MMS data packet header: %v", err)
		return
	}

	if _, err := conn.Write(packetHeaderBuf.Bytes()); err != nil {
		log.Printf("Error sending MMS data packet header: %v", err)
		return
	}

	// Send actual ASF header data
	if _, err := conn.Write(headerBuf[:n]); err != nil {
		log.Printf("Error sending ASF header data: %v", err)
		return
	}

	// Signal header end
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

		// Send as MMS data packet using proper MS-MMS command ID
		header = MMSHeader{
			CommandID:   MMS_LinkMacToViewerDataPacket, // Standard MS-MMS command ID
			Reserved1:   0,
			Reserved2:   0,
			MessageLen:  uint32(n + 40), // Add header size
			SequenceNum: packetNumber,   // Increment for each packet
			TimeoutVal:  0,
			Reserved3:   0,
			Reserved4:   0,
			Reserved5:   0,
			MessageLen2: uint32(n + 40),
		}

		// Send each packet with proper header
		dataBuf := new(bytes.Buffer)
		if err := binary.Write(dataBuf, binary.LittleEndian, header); err != nil {
			log.Printf("Error creating MMS data packet header: %v", err)
			break
		}

		if _, err := conn.Write(dataBuf.Bytes()); err != nil {
			log.Printf("Error sending MMS data packet header: %v", err)
			break
		}

		// Send the actual data
		if _, err := conn.Write(buffer[:n]); err != nil {
			log.Printf("Error sending MMS data packet data: %v", err)
			break
		}

		packetNumber++

		// Small delay between packets to prevent overwhelming the client
		// Adjust this value based on network conditions and client capabilities
		time.Sleep(20 * time.Millisecond)
	}

	// Send end of stream marker with proper MS-MMS command ID
	header = MMSHeader{
		CommandID:   MMS_LinkMacToViewerEndOfStream, // Standard MS-MMS command ID
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  40,
		SequenceNum: packetNumber, // Use next sequence number
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 40,
	}

	endBuf := new(bytes.Buffer)
	if err := binary.Write(endBuf, binary.LittleEndian, header); err != nil {
		log.Printf("Error creating MMS end of stream header: %v", err)
		return
	}

	if _, err := conn.Write(endBuf.Bytes()); err != nil {
		log.Printf("Error sending MMS end of stream header: %v", err)
		return
	}

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

// Send MMS file opened response
func sendMMSFileOpenedResponse(conn net.Conn, seqNum uint32) {
	response := MMSHeader{
		CommandID:   MMS_LinkMacToViewerReportOpenFile,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  40,
		SequenceNum: seqNum + 1,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 40,
	}

	respBuf := new(bytes.Buffer)
	if err := binary.Write(respBuf, binary.LittleEndian, response); err != nil {
		log.Printf("Error creating MMS file opened response: %v", err)
		return
	}

	if _, err := conn.Write(respBuf.Bytes()); err != nil {
		log.Printf("Error sending MMS file opened response: %v", err)
		return
	}

	log.Printf("Sent MMS file opened response (0x%08X)", MMS_LinkMacToViewerReportOpenFile)
}

// Send MMS start playing response
func sendMMSStartPlayingResponse(conn net.Conn, seqNum uint32) {
	response := MMSHeader{
		CommandID:   MMS_LinkMacToViewerReportStartPlaying,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  40,
		SequenceNum: seqNum + 1,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 40,
	}

	respBuf := new(bytes.Buffer)
	if err := binary.Write(respBuf, binary.LittleEndian, response); err != nil {
		log.Printf("Error creating MMS start playing response: %v", err)
		return
	}

	if _, err := conn.Write(respBuf.Bytes()); err != nil {
		log.Printf("Error sending MMS start playing response: %v", err)
		return
	}

	log.Printf("Sent MMS start playing response (0x%08X)", MMS_LinkMacToViewerReportStartPlaying)
}

// Send MMS stop playing response
func sendMMSStopPlayingResponse(conn net.Conn, seqNum uint32) {
	response := MMSHeader{
		CommandID:   MMS_LinkMacToViewerReportStopPlaying,
		Reserved1:   0,
		Reserved2:   0,
		MessageLen:  40,
		SequenceNum: seqNum + 1,
		TimeoutVal:  0,
		Reserved3:   0,
		Reserved4:   0,
		Reserved5:   0,
		MessageLen2: 40,
	}

	respBuf := new(bytes.Buffer)
	if err := binary.Write(respBuf, binary.LittleEndian, response); err != nil {
		log.Printf("Error creating MMS stop playing response: %v", err)
		return
	}

	if _, err := conn.Write(respBuf.Bytes()); err != nil {
		log.Printf("Error sending MMS stop playing response: %v", err)
		return
	}

	log.Printf("Sent MMS stop playing response (0x%08X)", MMS_LinkMacToViewerReportStopPlaying)
}

// Send a preloaded ASF header packet to the client
func sendPreloadedASFHeader(conn net.Conn, clientState *MMSClientState) {
	// In a real implementation, we might cache ASF headers
	// For now, we'll generate a minimal ASF header placeholder
	log.Printf("Sending ASF header for channel: %s", clientState.Channel.Name)

	// Send header start notification
	sendMMSHeaderStart(conn)

	// Typically, we'd extract this from the actual media file
	// This is just a placeholder - real implementation would get proper ASF headers
	asf_header_placeholder := []byte{
		0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11,
		0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C,
		// ... more ASF header bytes would go here
	}

	// Send the ASF header as data packet
	sendMMSDataPacket(conn, asf_header_placeholder, 0)

	// Send header end notification
	sendMMSHeaderEnd(conn)
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

	log.Printf("ASX request for channel: %s from %s", channel.Name, r.RemoteAddr)

	// Set proper content type with explicit charset
	w.Header().Set("Content-Type", "video/x-ms-asf; charset=utf-8")

	// Set no-cache to ensure fresh content
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Create ASX content
	content := generateASX(*channel, r.Host)

	// Set explicit content length
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))

	// Write the content
	w.Write([]byte(content))
}

// Generate ASX content as a string to allow setting proper content length
func generateASX(channel Channel, host string) string {
	// Extract host without port if necessary
	hostOnly := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	}

	// Create a Windows Media Player compatible ASX file with MMS protocol
	return fmt.Sprintf(`<ASX VERSION="3.0">
<ENTRY>
<TITLE>%s</TITLE>
<REF HREF="%s%s:%d/%s.wmv"/>
</ENTRY>
</ASX>`, channel.Name, MMSProtocol, hostOnly, MMSPort, channel.Slug)
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

	// Set up HTTP server for the web interface
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/", streamHandler)
	mux.HandleFunc("/asx/", asxHandler)

	// Improved file server for screenshots with better error handling
	screenshotsPath := "screenshots"
	fileServer := http.FileServer(http.Dir(screenshotsPath))
	mux.HandleFunc("/screenshots/", func(w http.ResponseWriter, r *http.Request) {
		// Log request for debugging
		log.Printf("Screenshot request: %s", r.URL.Path)

		// Add proper headers for images
		w.Header().Set("Cache-Control", "max-age=3600")

		// Strip the /screenshots/ prefix before passing to file server
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/screenshots/")

		// Check if file exists before serving
		filePath := filepath.Join(screenshotsPath, r.URL.Path)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			log.Printf("Screenshot not found: %s", filePath)
			http.NotFound(w, r)
			return
		}

		// Serve the file
		fileServer.ServeHTTP(w, r)
	})

	// Set up index page handler
	tmpl := template.Must(template.New("index").Parse(indexHTML))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only serve index at root path to prevent conflicts
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		// Set proper content type for HTML
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		// Force no caching for dynamic content
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		data := TemplateData{
			Channels: channels,
			Time:     time.Now().Format("2006-01-02 15:04:05"),
		}

		// Use buffer to render template first to avoid partial renders on error
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			http.Error(w, fmt.Sprintf("Error rendering template: %v", err), http.StatusInternalServerError)
			log.Printf("Template error: %v", err)
			return
		}

		// If template rendered successfully, write to response
		buf.WriteTo(w)
	})

	// Create a separate server instance
	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	log.Printf("Web server starting on http://localhost:8080")
	log.Printf("MMS server running on port %d", MMSPort)

	// Start the HTTP server
	log.Fatal(server.ListenAndServe())
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
