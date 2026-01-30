package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Config holds server configuration
type Config struct {
	ControlPort string
	WebPort     string
	Token       string
	WebUser     string
	WebPass     string
}

type ClientInfo struct {
	ID          string
	Hostname    string // Tên máy client
	Version     string // Client version
	RemoteAddr  string
	ConnectTime time.Time
	Latency     time.Duration
	session     *yamux.Session
	mu          sync.Mutex // Protects Latency
}

type ClientJSON struct {
	ID          string `json:"id"`
	Hostname    string `json:"hostname"`
	Version     string `json:"version"`
	RemoteAddr  string `json:"remote_addr"`
	ConnectTime string `json:"connect_time"`
	Duration    string `json:"duration"`
	Latency     string `json:"latency"`
}

type TunnelListener struct {
	Port   string
	Target string
}

// PortMapping lưu cấu hình port cho mỗi hostname
type PortMapping struct {
	Hostname string `json:"hostname"`
	Port     string `json:"port"`
	Target   string `json:"target"`
}

// UpdateInfo lưu thông tin về bản update client
type UpdateInfo struct {
	Version     string    `json:"version"`
	UploadTime  time.Time `json:"upload_time"`
	FileName    string    `json:"file_name"`
	FileSize    int64     `json:"file_size"`
	Description string    `json:"description"`
}

type TunnelServer struct {
	clients      map[string]*ClientInfo
	mutex        sync.RWMutex
	config       Config
	listeners    map[string]net.Listener
	listenerMeta map[string]string // port -> target
	listMutex    sync.Mutex
	startTime    time.Time
	logs         []string
	logMutex     sync.Mutex
	portMappings map[string]*PortMapping // hostname -> PortMapping
	mappingFile  string
	// Update related
	updateInfo      *UpdateInfo
	updateMutex     sync.RWMutex
	updateBinary    []byte
	updateInfoFile  string
	updateBinaryDir string
}

var server *TunnelServer

func main() {
	controlPort := flag.String("p", "7000", "Control port")
	webPort := flag.String("w", "8081", "Web Dashboard port")
	token := flag.String("t", "secret", "Client Token")
	webUser := flag.String("user", "admin", "Dashboard Username")
	webPass := flag.String("pass", "admin", "Dashboard Password")
	flag.Parse()

	server = &TunnelServer{
		clients:      make(map[string]*ClientInfo),
		listeners:    make(map[string]net.Listener),
		listenerMeta: make(map[string]string),
		portMappings: make(map[string]*PortMapping),
		mappingFile:  "port_mappings.json",
		config: Config{
			ControlPort: *controlPort,
			WebPort:     *webPort,
			Token:       *token,
			WebUser:     *webUser,
			WebPass:     *webPass,
		},
		startTime:       time.Now(),
		logs:            make([]string, 0, 100),
		updateInfoFile:  "update_info.json",
		updateBinaryDir: "updates",
	}

	// Load saved port mappings
	server.loadPortMappings()

	// Load update info
	server.loadUpdateInfo()

	server.addLog("INFO", "Server initializing...")
	server.addLog("INFO", fmt.Sprintf("Control port: %s", *controlPort))
	server.addLog("INFO", fmt.Sprintf("Web dashboard port: %s", *webPort))

	go server.startControlServer()
	go server.monitorLatency()
	server.startWebDashboard()
}

func (s *TunnelServer) startControlServer() {
	ln, err := net.Listen("tcp", ":"+s.config.ControlPort)
	if err != nil {
		log.Fatalf("❌ Control Server Error: %v", err)
	}
	log.Printf("🚀 Control Server on :%s", s.config.ControlPort)

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go s.handleClient(conn)
	}
}

func (s *TunnelServer) handleClient(conn net.Conn) {
	// TCP Optimization
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetReadBuffer(8 * 1024 * 1024)  // 8MB (tăng từ 4MB)
		tcpConn.SetWriteBuffer(8 * 1024 * 1024) // 8MB
	}

	// Read token
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)

	token, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	token = strings.TrimSpace(token)

	if token != s.config.Token {
		conn.Close()
		return
	}

	// Read hostname
	hostname, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		hostname = "unknown"
	}

	// Read version
	version, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	version = strings.TrimSpace(version)
	if version == "" {
		version = "unknown"
	}

	conn.SetReadDeadline(time.Time{})

	// Yamux Optimization
	muxConfig := yamux.DefaultConfig()
	muxConfig.KeepAliveInterval = 15 * time.Second
	muxConfig.MaxStreamWindowSize = 8 * 1024 * 1024 // 8MB Window (tăng từ 1MB)

	session, err := yamux.Server(conn, muxConfig)
	if err != nil {
		conn.Close()
		return
	}

	clientID := fmt.Sprintf("CLT-%d", time.Now().UnixNano()%10000)
	client := &ClientInfo{
		ID:          clientID,
		Hostname:    hostname,
		Version:     version,
		session:     session,
		RemoteAddr:  conn.RemoteAddr().String(),
		ConnectTime: time.Now(),
		Latency:     0,
	}

	s.mutex.Lock()
	s.clients[clientID] = client
	s.mutex.Unlock()

	log.Printf("✅ Client %s (%s) v%s connected from %s", clientID, hostname, version, client.RemoteAddr)
	s.addLog("INFO", fmt.Sprintf("Client %s (%s) v%s connected from %s", clientID, hostname, version, conn.RemoteAddr()))

	go func() {
		<-session.CloseChan()
		s.removeClient(clientID)
	}()
}

func (s *TunnelServer) monitorLatency() {
	for {
		time.Sleep(3 * time.Second)
		s.mutex.RLock()
		// Copy IDs to avoid holding lock during ping
		clients := make([]*ClientInfo, 0, len(s.clients))
		for _, c := range s.clients {
			clients = append(clients, c)
		}
		s.mutex.RUnlock()

		for _, c := range clients {
			go func(client *ClientInfo) {
				start := time.Now()
				_, err := client.session.Ping()
				if err == nil {
					client.mu.Lock()
					client.Latency = time.Since(start)
					client.mu.Unlock()
				}
			}(c)
		}
	}
}

func (s *TunnelServer) removeClient(id string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, ok := s.clients[id]; ok {
		delete(s.clients, id)
		log.Printf("👋 Client %s disconnected", id)
		s.addLog("INFO", fmt.Sprintf("Client %s disconnected", id))
	}
}

// addLog adds a log entry with timestamp
func (s *TunnelServer) addLog(level, message string) {
	s.logMutex.Lock()
	defer s.logMutex.Unlock()

	timestamp := time.Now().Format("15:04:05")
	logEntry := fmt.Sprintf("[%s] [%s] %s", timestamp, level, message)

	s.logs = append(s.logs, logEntry)

	// Keep only last 100 logs
	if len(s.logs) > 100 {
		s.logs = s.logs[len(s.logs)-100:]
	}
}

// loadPortMappings loads saved port mappings from file
func (s *TunnelServer) loadPortMappings() {
	data, err := os.ReadFile(s.mappingFile)
	if err != nil {
		// File doesn't exist yet, that's ok
		return
	}

	var mappings []PortMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		log.Printf("⚠️ Failed to load port mappings: %v", err)
		return
	}

	for _, m := range mappings {
		s.portMappings[m.Hostname] = &m
	}
	log.Printf("📂 Loaded %d port mapping(s)", len(mappings))
}

// savePortMappings saves current port mappings to file
func (s *TunnelServer) savePortMappings() {
	mappings := make([]PortMapping, 0, len(s.portMappings))
	for _, m := range s.portMappings {
		mappings = append(mappings, *m)
	}

	data, err := json.MarshalIndent(mappings, "", "  ")
	if err != nil {
		log.Printf("⚠️ Failed to marshal port mappings: %v", err)
		return
	}

	if err := os.WriteFile(s.mappingFile, data, 0644); err != nil {
		log.Printf("⚠️ Failed to save port mappings: %v", err)
		return
	}
}

// getPortMapping returns saved port mapping for a hostname
func (s *TunnelServer) getPortMapping(hostname string) *PortMapping {
	return s.portMappings[hostname]
}

// loadUpdateInfo loads update information from file
func (s *TunnelServer) loadUpdateInfo() {
	data, err := os.ReadFile(s.updateInfoFile)
	if err != nil {
		// File doesn't exist yet, that's ok
		return
	}

	var info UpdateInfo
	if err := json.Unmarshal(data, &info); err != nil {
		log.Printf("⚠️ Failed to load update info: %v", err)
		return
	}

	s.updateMutex.Lock()
	s.updateInfo = &info
	s.updateMutex.Unlock()

	// Load binary file
	binaryPath := fmt.Sprintf("%s/%s", s.updateBinaryDir, info.FileName)
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		log.Printf("⚠️ Update binary not found: %v", err)
		return
	}

	s.updateMutex.Lock()
	s.updateBinary = binary
	s.updateMutex.Unlock()

	log.Printf("📦 Loaded update v%s (%d bytes)", info.Version, len(binary))
}

// saveUpdateInfo saves update information to file
func (s *TunnelServer) saveUpdateInfo() error {
	s.updateMutex.RLock()
	defer s.updateMutex.RUnlock()

	if s.updateInfo == nil {
		return fmt.Errorf("no update info to save")
	}

	data, err := json.MarshalIndent(s.updateInfo, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.updateInfoFile, data, 0644)
}

// --- Web Middleware ---

func (s *TunnelServer) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(s.config.WebUser)) != 1 || subtle.ConstantTimeCompare([]byte(pass), []byte(s.config.WebPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Tunnel Pro Admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// --- API & UI ---

func (s *TunnelServer) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	s.listMutex.Lock()
	defer s.listMutex.Unlock()

	activeClients := make([]ClientJSON, 0)
	for _, c := range s.clients {
		c.mu.Lock()
		lat := c.Latency
		c.mu.Unlock()
		activeClients = append(activeClients, ClientJSON{
			ID:          c.ID,
			Hostname:    c.Hostname,
			Version:     c.Version,
			RemoteAddr:  c.RemoteAddr,
			ConnectTime: c.ConnectTime.Format("15:04:05"),
			Duration:    time.Since(c.ConnectTime).Round(time.Second).String(),
			Latency:     lat.Round(time.Millisecond).String(),
		})
	}

	activeListeners := make([]map[string]string, 0)
	for port := range s.listeners {
		target := s.listenerMeta[port]
		activeListeners = append(activeListeners, map[string]string{
			"port":   port,
			"target": target,
		})
	}

	s.logMutex.Lock()
	logsCopy := make([]string, len(s.logs))
	copy(logsCopy, s.logs)
	s.logMutex.Unlock()

	// Get saved port mappings
	savedMappings := make(map[string]map[string]string)
	for hostname, mapping := range s.portMappings {
		savedMappings[hostname] = map[string]string{
			"port":   mapping.Port,
			"target": mapping.Target,
		}
	}

	// Get update info
	s.updateMutex.RLock()
	var updateInfo map[string]interface{}
	if s.updateInfo != nil {
		updateInfo = map[string]interface{}{
			"version":     s.updateInfo.Version,
			"upload_time": s.updateInfo.UploadTime.Format("2006-01-02 15:04:05"),
			"file_size":   s.updateInfo.FileSize,
			"description": s.updateInfo.Description,
		}
	}
	s.updateMutex.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"clients":   activeClients,
		"listeners": activeListeners,
		"uptime":    time.Since(s.startTime).Round(time.Second).String(),
		"logs":      logsCopy,
		"mappings":  savedMappings,
		"update":    updateInfo,
	})
}

const dashboardHTML = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>TunnelHub Enterprise</title>
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700&display=swap" rel="stylesheet">
    <style>
        :root {
            /* Enterprise Palette - Slate/Blue */
            --bg-body: #0f172a;      /* Slate 900 */
            --bg-sidebar: #1e293b;   /* Slate 800 */
            --bg-card: #1e293b;      /* Slate 800 */
            --bg-input: #334155;     /* Slate 700 */
            --border-color: #334155; /* Slate 700 */
            
            --text-main: #f8fafc;    /* Slate 50 */
            --text-muted: #94a3b8;   /* Slate 400 */
            
            --primary: #3b82f6;      /* Blue 500 */
            --primary-hover: #2563eb;/* Blue 600 */
            --danger: #ef4444;       /* Red 500 */
            --success: #10b981;      /* Emerald 500 */
            --warning: #f59e0b;      /* Amber 500 */

            --shadow-sm: 0 1px 2px 0 rgb(0 0 0 / 0.05);
            --shadow-md: 0 4px 6px -1px rgb(0 0 0 / 0.1), 0 2px 4px -2px rgb(0 0 0 / 0.1);
            --radius: 0.5rem;
        }

        * { box-sizing: border-box; outline: none; }
        body { 
            margin: 0; 
            font-family: 'Inter', sans-serif; 
            background-color: var(--bg-body); 
            color: var(--text-main);
            height: 100vh;
            display: flex;
            overflow: hidden;
        }

        /* Sidebar */
        .sidebar {
            width: 260px;
            background-color: var(--bg-sidebar);
            border-right: 1px solid var(--border-color);
            display: flex;
            flex-direction: column;
            padding: 1.5rem;
            flex-shrink: 0;
        }
        .brand {
            font-size: 1.25rem;
            font-weight: 700;
            color: var(--text-main);
            display: flex;
            align-items: center;
            gap: 0.75rem;
            margin-bottom: 2.5rem;
        }
        .brand span { color: var(--primary); }
        .nav-item {
            display: flex;
            align-items: center;
            gap: 0.75rem;
            padding: 0.75rem 1rem;
            color: var(--text-muted);
            text-decoration: none;
            border-radius: var(--radius);
            margin-bottom: 0.5rem;
            transition: all 0.2s;
            font-weight: 500;
        }
        .nav-item:hover, .nav-item.active {
            background-color: rgba(59, 130, 246, 0.1);
            color: var(--primary);
        }
        .nav-icon { width: 20px; height: 20px; text-align: center; }

        /* Main Content */
        .main-content {
            flex: 1;
            overflow-y: auto;
            padding: 2rem;
            position: relative;
        }

        .header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 2rem;
        }
        .header h1 { margin: 0; font-size: 1.5rem; font-weight: 600; }
        .user-profile {
            display: flex;
            align-items: center;
            gap: 1rem;
        }
        .avatar {
            width: 36px;
            height: 36px;
            background: var(--primary);
            border-radius: 50%;
            display: flex;
            align-items: center;
            justify-content: center;
            font-weight: 600;
            font-size: 0.9rem;
        }

        /* Cards & Grid */
        .grid {
            display: grid;
            grid-template-columns: repeat(12, 1fr);
            gap: 1.5rem;
        }
        .col-full { grid-column: span 12; }
        .col-half { grid-column: span 6; }
        
        .card {
            background-color: var(--bg-card);
            border: 1px solid var(--border-color);
            border-radius: var(--radius);
            box-shadow: var(--shadow-sm);
            overflow: hidden;
            display: flex;
            flex-direction: column;
        }
        
        .card-header {
            padding: 1.25rem 1.5rem;
            border-bottom: 1px solid var(--border-color);
            display: flex;
            justify-content: space-between;
            align-items: center;
        }
        .card-header h3 { margin: 0; font-size: 1rem; font-weight: 600; color: var(--text-main); }
        .card-body { padding: 1.5rem; overflow-x: auto; }

        /* Tables */
        table { width: 100%; border-collapse: collapse; font-size: 0.875rem; }
        th { 
            text-align: left; 
            padding: 0.75rem 1rem; 
            color: var(--text-muted); 
            font-weight: 500; 
            border-bottom: 1px solid var(--border-color);
        }
        td { 
            padding: 0.75rem 1rem; 
            border-bottom: 1px solid var(--border-color); 
            color: var(--text-main);
            vertical-align: middle;
        }
        tr:last-child td { border-bottom: none; }
        tr:hover td { background-color: rgba(255,255,255,0.02); }

        /* Badges & Indicators */
        .badge {
            display: inline-flex;
            align-items: center;
            padding: 0.25rem 0.625rem;
            border-radius: 9999px;
            font-size: 0.75rem;
            font-weight: 500;
        }
        .badge-gray { background: var(--bg-input); color: var(--text-muted); }
        .badge-green { background: rgba(16, 185, 129, 0.1); color: var(--success); }
        .badge-red { background: rgba(239, 68, 68, 0.1); color: var(--danger); }
        .ping-indicator { font-weight: 600; font-variant-numeric: tabular-nums; }
        .text-green { color: var(--success); }
        .text-warning { color: var(--warning); }
        .text-danger { color: var(--danger); }

        /* Forms & Buttons */
        .input-group {
            display: flex;
            gap: 0.5rem;
            align-items: center;
        }
        input {
            background-color: var(--bg-input);
            border: 1px solid var(--border-color);
            color: var(--text-main);
            padding: 0.5rem 0.75rem;
            border-radius: var(--radius);
            font-size: 0.875rem;
            transition: border-color 0.2s;
            width: 100%;
        }
        input:focus { border-color: var(--primary); }
        
        button {
            display: inline-flex;
            align-items: center;
            justify-content: center;
            padding: 0.5rem 1rem;
            border-radius: var(--radius);
            font-size: 0.875rem;
            font-weight: 500;
            cursor: pointer;
            transition: all 0.2s;
            border: none;
        }
        .btn-primary { background-color: var(--primary); color: white; }
        .btn-primary:hover { background-color: var(--primary-hover); }
        .btn-danger { background-color: rgba(239,68,68,0.1); color: var(--danger); }
        .btn-danger:hover { background-color: var(--danger); color: white; }
        .btn-sm { padding: 0.375rem 0.75rem; font-size: 0.75rem; }

        /* Loading & Empty States */
        .empty-state {
            padding: 3rem;
            text-align: center;
            color: var(--text-muted);
            font-size: 0.875rem;
        }
        .loading { opacity: 0.6; pointer-events: none; }
        
        /* Page switching */
        .page { display: none; }
        .page.active { display: block; }
        
        @media (max-width: 768px) {
            .sidebar { display: none; }
            .col-half { grid-column: span 12; }
            .grid { gap: 1rem; }
            .main-content { padding: 1rem; }
        }
    </style>
</head>
<body>
    <!-- Sidebar -->
    <aside class="sidebar">
        <div class="brand">
            <span>⚡</span> TunnelHub
        </div>
        <nav>
            <a href="#" class="nav-item active" onclick="showPage('dashboard'); return false;">
                <span class="nav-icon">📊</span> Dashboard
            </a>
            <a href="#" class="nav-item" onclick="showPage('settings'); return false;">
                <span class="nav-icon">⚙️</span> Settings
            </a>
            <a href="#" class="nav-item" onclick="showPage('logs'); return false;">
                <span class="nav-icon">📄</span> Logs
            </a>
        </nav>
        <div style="flex:1"></div>
        <div class="nav-item" style="cursor: default;">
            <span class="nav-icon">🔒</span> Secure Mode
        </div>
    </aside>

    <!-- Main Content -->
    <main class="main-content">
        <!-- Dashboard Page -->
        <div id="page-dashboard" class="page active">
            <div class="header">
                <h1>Analytics Overview</h1>
                <div class="user-profile">
                    <span style="font-size: 0.875rem; color: var(--text-muted);">Admin Portal</span>
                    <div class="avatar">A</div>
                </div>
            </div>

            <div class="grid">
                <!-- Connected Clients Card -->
                <div class="card col-full">
                    <div class="card-header">
                        <h3>Connected Clients</h3>
                        <div class="badge badge-gray" id="client-count">0 Online</div>
                    </div>
                    <div class="card-body">
                        <div id="clients-list">
                            <div class="empty-state">Waiting for secure connections...</div>
                        </div>
                    </div>
                </div>

                <!-- Active Tunnels Card -->
                <div class="card col-full">
                    <div class="card-header">
                        <h3>Active Tunnels</h3>
                        <div class="badge badge-green" id="tunnel-count">0 Active</div>
                    </div>
                    <div class="card-body">
                        <div id="tunnels-list">
                            <div class="empty-state">No active tunnels. Open a port above.</div>
                        </div>
                    </div>
                </div>
            </div>
        </div>

        <!-- Settings Page -->
        <div id="page-settings" class="page">
            <div class="header">
                <h1>Server Configuration</h1>
            </div>
            <div class="grid">
                <div class="card col-full">
                    <div class="card-header">
                        <h3>Server Information</h3>
                    </div>
                    <div class="card-body">
                        <table>
                            <tr><td style="width: 30%; font-weight: 500;">Control Port</td><td id="info-control-port">7000</td></tr>
                            <tr><td style="font-weight: 500;">Web Dashboard Port</td><td id="info-web-port">8081</td></tr>
                            <tr><td style="font-weight: 500;">Authentication</td><td><span class="badge badge-green">Enabled</span></td></tr>
                            <tr><td style="font-weight: 500;">Server Uptime</td><td id="info-uptime">-</td></tr>
                        </table>
                    </div>
                </div>
            </div>
        </div>

        <!-- Logs Page -->
        <div id="page-logs" class="page">
            <div class="header">
                <h1>System Logs</h1>
            </div>
            <div class="grid">
                <div class="card col-full">
                    <div class="card-header">
                        <h3>Recent Activity</h3>
                    </div>
                    <div class="card-body">
                        <div style="font-family: monospace; font-size: 0.8rem; background: var(--bg-input); padding: 1rem; border-radius: 0.5rem; max-height: 400px; overflow-y: auto;" id="log-content">
                            <div style="color: var(--success);">[INFO] Server started successfully</div>
                            <div style="color: var(--text-muted);">[INFO] Listening on port 7000</div>
                            <div style="color: var(--text-muted);">[INFO] Web dashboard available at :8081</div>
                        </div>
                    </div>
                </div>
            </div>
        </div>
    </main>

    <script>
        // State management
        let isEditing = false;
        let savedMappings = {}; // Store saved port mappings

        document.addEventListener('focusin', function(e) {
            if (e.target.tagName === 'INPUT') isEditing = true;
        });

        document.addEventListener('focusout', function(e) {
            if (e.target.tagName === 'INPUT') isEditing = false;
        });

        // Data Fetching
        function updateStatus() {
            if (isEditing) return;

            fetch('/api/status')
                .then(function(r) { return r.json(); })
                .then(function(data) {
                    // Store mappings
                    if (data.mappings) {
                        savedMappings = data.mappings;
                    }
                    
                    renderClients(data.clients);
                    renderTunnels(data.listeners);
                    
                    // Update header counters
                    document.getElementById('client-count').textContent = (data.clients ? data.clients.length : 0) + ' Online';
                    document.getElementById('tunnel-count').textContent = (data.listeners ? data.listeners.length : 0) + ' Active';
                    
                    // Update uptime
                    if (document.getElementById('info-uptime')) {
                        document.getElementById('info-uptime').textContent = data.uptime || '-';
                    }
                    
                    // Update logs
                    if (data.logs && document.getElementById('log-content')) {
                        var logHtml = '';
                        for (var i = data.logs.length - 1; i >= 0; i--) {
                            var log = data.logs[i];
                            var color = 'var(--text-muted)';
                            if (log.indexOf('[ERROR]') > -1) color = 'var(--danger)';
                            else if (log.indexOf('[WARN]') > -1) color = 'var(--warning)';
                            else if (log.indexOf('[INFO]') > -1) color = 'var(--success)';
                            logHtml += '<div style="color: ' + color + ';">' + log + '</div>';
                        }
                        document.getElementById('log-content').innerHTML = logHtml || '<div style="color: var(--text-muted);">No logs available</div>';
                    }
                })
                .catch(function(err) { console.error("Connection lost", err); });
        }

        function renderClients(clients) {
            const container = document.getElementById('clients-list');
            if (!clients || clients.length === 0) {
                container.innerHTML = '<div class="empty-state">No clients connected.</div>';
                return;
            }
            
            let html = '<table>' +
                '<thead>' +
                    '<tr>' +
                        '<th style="width: 25%">Client Identity</th>' +
                        '<th style="width: 20%">Connection Info</th>' +
                        '<th style="width: 15%">Latency</th>' +
                        '<th style="width: 40%">Tunnel Controls</th>' +
                    '</tr>' +
                '</thead>' +
                '<tbody>';
            
            clients.forEach(function(c) {
                // Check for saved mapping for this hostname
                var savedPort = '';
                var savedTarget = 'localhost:80';
                if (savedMappings[c.hostname]) {
                    savedPort = savedMappings[c.hostname].port || '';
                    savedTarget = savedMappings[c.hostname].target || 'localhost:80';
                }

                // Preserve input values if user is editing
                const existingPort = document.getElementById('p-'+c.id);
                const existingTarget = document.getElementById('t-'+c.id);
                const portVal = existingPort ? existingPort.value : savedPort;
                const targetVal = existingTarget ? existingTarget.value : savedTarget;

                const latVal = parseInt(c.latency);
                let pingColor = 'text-green';
                if (latVal > 100) pingColor = 'text-warning';
                if (latVal > 200) pingColor = 'text-danger';

                html += '<tr>' +
                    '<td>' +
                        '<div style="font-weight: 600; font-size: 0.95rem;">' + c.hostname + '</div>' +
                        '<div style="font-size: 0.75rem; color: var(--text-muted);">' + c.id + '</div>' +
                        '<div style="font-size: 0.7rem; color: var(--text-muted); margin-top: 2px;">' + c.remote_addr + '</div>' +
                    '</td>' +
                    '<td>' +
                        '<div class="badge badge-gray">' + c.duration + '</div>' +
                    '</td>' +
                    '<td>' +
                        '<span class="ping-indicator ' + pingColor + '">📶 ' + c.latency + '</span>' +
                    '</td>' +
                    '<td>' +
                        '<div class="input-group">' +
                            '<input type="number" id="p-' + c.id + '" placeholder="Public Port" value="' + portVal + '" style="width: 100px;">' +
                            '<span style="color: var(--text-muted);">&rarr;</span>' +
                            '<input type="text" id="t-' + c.id + '" placeholder="Target (127.0.0.1:80)" value="' + targetVal + '">' +
                            '<button class="btn-primary btn-sm" onclick="openTunnel(\'' + c.id + '\')">Open</button>' +
                        '</div>' +
                    '</td>' +
                '</tr>';
            });
            
            html += '</tbody></table>';
            container.innerHTML = html;
        }

        function renderTunnels(tunnels) {
            const container = document.getElementById('tunnels-list');
            if (!tunnels || tunnels.length === 0) {
                container.innerHTML = '<div class="empty-state">No active tunnels found.</div>';
                return;
            }
            
            let html = '<table>' +
                '<thead>' +
                    '<tr>' +
                        '<th style="width: 30%">Public Endpoint</th>' +
                        '<th style="width: 10%">Direction</th>' +
                        '<th style="width: 40%">Internal Target</th>' +
                        '<th style="width: 20%">Actions</th>' +
                    '</tr>' +
                '</thead>' +
                '<tbody>';
                
            tunnels.forEach(function(t) {
                html += '<tr>' +
                    '<td>' +
                        '<div style="font-weight: 600; color: var(--primary);">0.0.0.0:' + t.port + '</div>' +
                    '</td>' +
                    '<td>' +
                        '<span style="color: var(--text-muted);">&rarr;</span>' +
                    '</td>' +
                    '<td>' +
                        '<div class="badge badge-gray">' + t.target + '</div>' +
                    '</td>' +
                    '<td>' +
                        '<button class="btn-danger btn-sm" onclick="closeTunnel(\'' + t.port + '\')">Terminate</button>' +
                    '</td>' +
                '</tr>';
            });
            
            html += '</tbody></table>';
            container.innerHTML = html;
        }

        function openTunnel(id) {
            const port = document.getElementById('p-'+id).value;
            const target = document.getElementById('t-'+id).value || "localhost:80";
            
            if(!port) {
                alert("Please specify a public port number.");
                return;
            }
            
            window.location.href = '/open?id=' + id + '&port=' + port + '&target=' + target;
        }

        function closeTunnel(port) {
            if(confirm('Are you sure you want to stop the tunnel on port ' + port + '?')) {
                const form = document.createElement('form');
                form.method = 'POST';
                form.action = '/close';
                
                const field = document.createElement('input');
                field.type = 'hidden';
                field.name = 'port';
                field.value = port;
                
                form.appendChild(field);
                document.body.appendChild(form);
                form.submit();
            }
        }

        // Page Navigation
        function showPage(pageName) {
            // Hide all pages
            var pages = document.querySelectorAll('.page');
            for (var i = 0; i < pages.length; i++) {
                pages[i].classList.remove('active');
            }
            
            // Show selected page
            document.getElementById('page-' + pageName).classList.add('active');
            
            // Update nav active state
            var navItems = document.querySelectorAll('.nav-item');
            for (var i = 0; i < navItems.length; i++) {
                navItems[i].classList.remove('active');
            }
            event.target.closest('.nav-item').classList.add('active');
        }

        // Initialize
        updateStatus();
        setInterval(updateStatus, 2000);
    </script>
</body>
</html>
`

func (s *TunnelServer) startWebDashboard() {
	tmpl := template.Must(template.New("dashboard").Parse(dashboardHTML))

	http.HandleFunc("/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		tmpl.Execute(w, nil)
	}))

	http.HandleFunc("/api/status", s.authMiddleware(s.handleAPIStatus))

	http.HandleFunc("/open", s.authMiddleware(s.handleOpenPort))
	http.HandleFunc("/close", s.authMiddleware(s.handleClosePort))

	// Update endpoints
	http.HandleFunc("/api/update/upload", s.authMiddleware(s.handleUploadUpdate))
	http.HandleFunc("/api/update/download", s.handleDownloadUpdate) // No auth - clients need this

	log.Printf("🌐 Dashboard: http://localhost:%s (User: %s)", s.config.WebPort, s.config.WebUser)
	log.Fatal(http.ListenAndServe(":"+s.config.WebPort, nil))
}

func (s *TunnelServer) handleOpenPort(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	portStr := r.URL.Query().Get("port")
	targetAddr := r.URL.Query().Get("target")

	s.mutex.RLock()
	client, ok := s.clients[id]
	s.mutex.RUnlock()

	if !ok {
		http.Error(w, "Client not found", 404)
		return
	}

	// Save port mapping for this hostname
	s.portMappings[client.Hostname] = &PortMapping{
		Hostname: client.Hostname,
		Port:     portStr,
		Target:   targetAddr,
	}
	s.savePortMappings()

	go func() {
		ln, err := net.Listen("tcp", ":"+portStr)
		if err != nil {
			log.Printf("❌ Failed to open port %s: %v", portStr, err)
			return
		}

		s.listMutex.Lock()
		s.listeners[portStr] = ln
		s.listenerMeta[portStr] = targetAddr
		s.listMutex.Unlock()

		defer func() {
			s.listMutex.Lock()
			delete(s.listeners, portStr)
			delete(s.listenerMeta, portStr)
			s.listMutex.Unlock()
			ln.Close()
		}()

		for {
			userConn, err := ln.Accept()
			if err != nil {
				return
			}

			// Speed Tune User Connection - 8MB buffers
			if tcpConn, ok := userConn.(*net.TCPConn); ok {
				tcpConn.SetNoDelay(true)
				tcpConn.SetReadBuffer(8 * 1024 * 1024)
				tcpConn.SetWriteBuffer(8 * 1024 * 1024)
			}

			go func() {
				stream, err := client.session.Open()
				if err != nil {
					userConn.Close()
					return
				}
				stream.Write([]byte(targetAddr + "\n"))

				// Buffered Copy for Speed - 128KB buffers
				buf1 := make([]byte, 128*1024)
				buf2 := make([]byte, 128*1024)

				go func() {
					io.CopyBuffer(stream, userConn, buf1)
					stream.Close()
				}()
				io.CopyBuffer(userConn, stream, buf2)
				userConn.Close()
			}()
		}
	}()

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *TunnelServer) handleClosePort(w http.ResponseWriter, r *http.Request) {
	port := r.FormValue("port")
	s.listMutex.Lock()
	if ln, ok := s.listeners[port]; ok {
		ln.Close()
	}
	s.listMutex.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *TunnelServer) handleUploadUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse multipart form (max 50MB)
	err := r.ParseMultipartForm(50 << 20)
	if err != nil {
		http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	version := r.FormValue("version")
	description := r.FormValue("description")

	if version == "" {
		http.Error(w, "Version is required", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("binary")
	if err != nil {
		http.Error(w, "Failed to get file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read file content
	binary, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Failed to read file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Create updates directory if not exists
	os.MkdirAll(s.updateBinaryDir, 0755)

	// Save binary file
	binaryPath := fmt.Sprintf("%s/%s", s.updateBinaryDir, header.Filename)
	err = os.WriteFile(binaryPath, binary, 0755)
	if err != nil {
		http.Error(w, "Failed to save binary: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Update info
	s.updateMutex.Lock()
	s.updateInfo = &UpdateInfo{
		Version:     version,
		UploadTime:  time.Now(),
		FileName:    header.Filename,
		FileSize:    int64(len(binary)),
		Description: description,
	}
	s.updateBinary = binary
	s.updateMutex.Unlock()

	// Save update info
	err = s.saveUpdateInfo()
	if err != nil {
		log.Printf("⚠️ Failed to save update info: %v", err)
	}

	log.Printf("📦 Update v%s uploaded (%d bytes)", version, len(binary))
	s.addLog("INFO", fmt.Sprintf("Update v%s uploaded (%d bytes)", version, len(binary)))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"version": version,
		"size":    len(binary),
	})
}

func (s *TunnelServer) handleDownloadUpdate(w http.ResponseWriter, r *http.Request) {
	s.updateMutex.RLock()
	defer s.updateMutex.RUnlock()

	if s.updateInfo == nil || s.updateBinary == nil {
		http.Error(w, "No update available", http.StatusNotFound)
		return
	}

	// Return version info if requested
	if r.URL.Query().Get("check") == "1" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"version":     s.updateInfo.Version,
			"file_size":   s.updateInfo.FileSize,
			"description": s.updateInfo.Description,
		})
		return
	}

	// Download binary
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", s.updateInfo.FileName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(s.updateBinary)))
	w.Write(s.updateBinary)
}
