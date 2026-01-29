package main

import (
	"bufio"
	"flag"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kardianos/service"
)

// Config holds client configuration
type Config struct {
	ServerAddr       string
	DefaultLocalAddr string
	Token            string
}

type program struct {
	cfg  Config
	exit chan struct{}
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

	err = s.Run()
	if err != nil {
		logger.Error(err)
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
	logger.Infof("Target Server: %s", p.cfg.ServerAddr)

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

	_, err = conn.Write([]byte(cfg.Token))
	if err != nil {
		return err
	}

	// Yamux Speed Optimization
	muxConfig := yamux.DefaultConfig()
	muxConfig.KeepAliveInterval = 15 * time.Second
	muxConfig.MaxStreamWindowSize = 1024 * 1024 // 1MB Window
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

	// Use io.CopyBuffer for throughput
	buf1 := make([]byte, 32*1024)
	buf2 := make([]byte, 32*1024)

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
