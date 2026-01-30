package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kardianos/service"
)

const CLIENT_VERSION = "2.0.0"

// Config holds client configuration
type Config struct {
	ServerAddr       string
	DefaultLocalAddr string
	Token            string
}

type program struct {
	cfg       Config
	exit      chan struct{}
	isService bool // Track if running as service
}

var logger service.Logger

func main() {
	// Flags
	serverAddr := flag.String("s", "127.0.0.1:7000", "Server address")
	localAddr := flag.String("l", "localhost:3389", "Default local app")
	token := flag.String("t", "secret", "Auth token")
	svcFlag := flag.String("service", "", "Control the system service: install, uninstall, start, stop")
	flag.Parse()

	svcConfig := &service.Config{
		Name:        "TunnelProClient",
		DisplayName: "Tunnel Pro Client",
		Description: "Secure Tunnel Client Service",
		Arguments:   []string{"-s", *serverAddr, "-l", *localAddr, "-t", *token},
	}

	prg := &program{
		cfg: Config{
			ServerAddr:       *serverAddr,
			DefaultLocalAddr: *localAddr,
			Token:            *token,
		},
		exit: make(chan struct{}),
	}

	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}

	if *svcFlag != "" {
		err = service.Control(s, *svcFlag)
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	logger, err = s.Logger(nil)
	if err != nil {
		log.Fatal(err)
	}

	// Detect if running as service
	prg.isService = !service.Interactive()

	err2 := s.Run()
	if err2 != nil {
		logger.Error(err2)
	}
}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) Stop(s service.Service) error {
	close(p.exit)
	return nil
}

func (p *program) run() {
	logger.Info("🚀 Tunnel Service Started")
	logger.Infof("Client Version: %s", CLIENT_VERSION)
	logger.Infof("Target Server: %s", p.cfg.ServerAddr)

	// Start update checker
	go p.checkForUpdates()

	backoff := time.Second
	for {
		select {
		case <-p.exit:
			return
		default:
			err := connectToServer(p.cfg)
			if err != nil {
				logger.Errorf("Connection failed: %v", err)
			} else {
				logger.Info("Connection lost. Reconnecting...")
				backoff = time.Second
			}

			time.Sleep(backoff)
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func connectToServer(cfg Config) error {
	conn, err := net.DialTimeout("tcp", cfg.ServerAddr, 5*time.Second)
	if err != nil {
		return err
	}

	// TCP Speed Optimization
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetReadBuffer(4 * 1024 * 1024)
		tcpConn.SetWriteBuffer(4 * 1024 * 1024)
	}
	defer conn.Close()

	// Send token
	_, err = conn.Write([]byte(cfg.Token + "\n"))
	if err != nil {
		return err
	}

	// Send hostname
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	_, err = conn.Write([]byte(hostname + "\n"))
	if err != nil {
		return err
	}

	// Send version
	_, err = conn.Write([]byte(CLIENT_VERSION + "\n"))
	if err != nil {
		return err
	}

	// Yamux Speed Optimization
	muxConfig := yamux.DefaultConfig()
	muxConfig.KeepAliveInterval = 15 * time.Second
	muxConfig.MaxStreamWindowSize = 8 * 1024 * 1024 // 8MB Window (tăng từ 1MB)
	muxConfig.ConnectionWriteTimeout = 10 * time.Second

	session, err := yamux.Client(conn, muxConfig)
	if err != nil {
		return err
	}
	defer session.Close()

	logger.Info("✅ Connected to Server")

	for {
		stream, err := session.Accept()
		if err != nil {
			return err
		}
		go handleStream(stream, cfg.DefaultLocalAddr)
	}
}

func handleStream(stream net.Conn, defaultLocalAddr string) {
	defer stream.Close()

	reader := bufio.NewReader(stream)
	targetAddr, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	targetAddr = strings.TrimSpace(targetAddr)
	if targetAddr == "" {
		targetAddr = defaultLocalAddr
	}

	localConn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		return
	}

	// TCP Speed for Local App
	if tcpConn, ok := localConn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetReadBuffer(4 * 1024 * 1024)
		tcpConn.SetWriteBuffer(4 * 1024 * 1024)
	}
	defer localConn.Close()

	// Use io.CopyBuffer for throughput - 128KB buffers for 1Gbps
	buf1 := make([]byte, 128*1024)
	buf2 := make([]byte, 128*1024)

	go func() {
		io.CopyBuffer(localConn, reader, buf1) // Need to handle buffered reader
		// Note: io.CopyBuffer on a Reader wrapper might lose the buffer benefit if Reader is small,
		// but here Reader has already consumed newlines.
		// Actually bufio.Reader can be written to.
		// Standard io.Copy is fine, bufio might actually be the bottleneck if not careful.
		// But we already used bufio to read the first line. We MUST read from 'reader'.
	}()
	io.CopyBuffer(stream, localConn, buf2)
}

func (p *program) checkForUpdates() {
	// Check every 30 minutes
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	// Also check immediately on startup (after 10 seconds)
	time.Sleep(10 * time.Second)
	p.performUpdateCheck()

	for {
		select {
		case <-p.exit:
			return
		case <-ticker.C:
			p.performUpdateCheck()
		}
	}
}

func (p *program) performUpdateCheck() {
	// Extract server address for HTTP
	serverHost := strings.Split(p.cfg.ServerAddr, ":")[0]
	// Assume web dashboard is on port 8081 (default)
	updateURL := fmt.Sprintf("http://%s:8081/api/update/download?check=1", serverHost)

	resp, err := http.Get(updateURL)
	if err != nil {
		// Server might not have update feature, silently ignore
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No update available
		return
	}

	if resp.StatusCode != http.StatusOK {
		return
	}

	var updateInfo struct {
		Version     string `json:"version"`
		FileSize    int64  `json:"file_size"`
		Description string `json:"description"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&updateInfo); err != nil {
		return
	}

	// Compare versions
	if updateInfo.Version == CLIENT_VERSION {
		// Already up to date
		return
	}

	logger.Infof("🔄 New update available: v%s (current: v%s)", updateInfo.Version, CLIENT_VERSION)
	logger.Infof("📝 %s", updateInfo.Description)
	logger.Info("⬇️ Downloading update...")

	// Download update
	downloadURL := fmt.Sprintf("http://%s:8081/api/update/download", serverHost)
	resp, err = http.Get(downloadURL)
	if err != nil {
		logger.Errorf("Failed to download update: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Errorf("Failed to download update: HTTP %d", resp.StatusCode)
		return
	}

	// Read new binary
	newBinary, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Errorf("Failed to read update: %v", err)
		return
	}

	logger.Infof("✅ Downloaded %d bytes", len(newBinary))

	// Get current executable path
	exePath, err := os.Executable()
	if err != nil {
		logger.Errorf("Failed to get executable path: %v", err)
		return
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	// Save new binary to temp file
	tempPath := exePath + ".new"
	err = os.WriteFile(tempPath, newBinary, 0755)
	if err != nil {
		logger.Errorf("Failed to write update: %v", err)
		return
	}

	logger.Info("🔄 Installing update and restarting...")

	// Create update script
	// On Windows, we need a batch script to replace the running exe
	scriptPath := exePath + ".update.bat"

	var script string
	if p.isService {
		// Running as service - restart service
		script = fmt.Sprintf(`@echo off
timeout /t 2 /nobreak > nul
move /y "%s" "%s"
del "%s"
sc stop TunnelProClient
timeout /t 2 /nobreak > nul
sc start TunnelProClient
del "%%~f0"
`, tempPath, exePath, scriptPath)
		logger.Info("📋 Update will restart Windows Service")
	} else {
		// Running as console - start console app
		script = fmt.Sprintf(`@echo off
timeout /t 2 /nobreak > nul
move /y "%s" "%s"
del "%s"
start "" "%s" -s "%s" -t "%s" -l "%s"
del "%%~f0"
`, tempPath, exePath, scriptPath, exePath, p.cfg.ServerAddr, p.cfg.Token, p.cfg.DefaultLocalAddr)
		logger.Info("📋 Update will restart Console App")
	}

	err = os.WriteFile(scriptPath, []byte(script), 0755)
	if err != nil {
		logger.Errorf("Failed to create update script: %v", err)
		return
	}

	// Execute update script
	cmd := exec.Command("cmd", "/c", scriptPath)
	cmd.Start()

	// Exit current process
	logger.Info("👋 Exiting for update...")
	close(p.exit)
	os.Exit(0)
}
