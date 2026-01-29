package main

import (
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
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
	RemoteAddr  string
	ConnectTime time.Time
	Latency     time.Duration
	session     *yamux.Session
	mu          sync.Mutex // Protects Latency
}

type ClientJSON struct {
	ID          string `json:"id"`
	RemoteAddr  string `json:"remote_addr"`
	ConnectTime string `json:"connect_time"`
	Duration    string `json:"duration"`
	Latency     string `json:"latency"`
}

type TunnelListener struct {
	Port   string
	Target string
}

type TunnelServer struct {
	clients      map[string]*ClientInfo
	mutex        sync.RWMutex
	config       Config
	listeners    map[string]net.Listener
	listenerMeta map[string]string // port -> target
	listMutex    sync.Mutex
	startTime    time.Time
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
		config: Config{
			ControlPort: *controlPort,
			WebPort:     *webPort,
			Token:       *token,
			WebUser:     *webUser,
			WebPass:     *webPass,
		},
		startTime: time.Now(),
	}

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
		tcpConn.SetReadBuffer(4 * 1024 * 1024)  // 4MB
		tcpConn.SetWriteBuffer(4 * 1024 * 1024) // 4MB
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})

	if strings.TrimSpace(string(buf[:n])) != s.config.Token {
		conn.Close()
		return
	}

	// Yamux Optimization
	muxConfig := yamux.DefaultConfig()
	muxConfig.KeepAliveInterval = 15 * time.Second
	muxConfig.MaxStreamWindowSize = 1024 * 1024 // 1MB Window

	session, err := yamux.Server(conn, muxConfig)
	if err != nil {
		conn.Close()
		return
	}

	clientID := fmt.Sprintf("CLT-%d", time.Now().UnixNano()%10000)
	client := &ClientInfo{
		ID:          clientID,
		session:     session,
		RemoteAddr:  conn.RemoteAddr().String(),
		ConnectTime: time.Now(),
		Latency:     0,
	}

	s.mutex.Lock()
	s.clients[clientID] = client
	s.mutex.Unlock()

	log.Printf("✅ Client %s connected from %s", clientID, client.RemoteAddr)

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
	}
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

	json.NewEncoder(w).Encode(map[string]interface{}{
		"clients":   activeClients,
		"listeners": activeListeners,
		"uptime":    time.Since(s.startTime).String(),
	})
}

const dashboardHTML = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Tunnel Pro Ultimate</title>
    <link href="https://fonts.googleapis.com/css2?family=Outfit:wght@300;400;600&display=swap" rel="stylesheet">
    <style>
        :root {
            --bg: #09090b;
            --surface: #18181b;
            --surface-highlight: #27272a;
            --border: #3f3f46;
            --primary: #6366f1;
            --primary-glow: rgba(99, 102, 241, 0.4);
            --text: #f4f4f5;
            --text-muted: #a1a1aa;
            --success: #10b981;
            --danger: #ef4444;
            --warning: #f59e0b;
        }
        * { box-sizing: border-box; }
        body { margin: 0; font-family: 'Outfit', sans-serif; background: var(--bg); color: var(--text); padding: 40px 20px; }
        .container { max-width: 1200px; margin: 0 auto; }
        
        header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 40px; }
        h1 { font-weight: 600; font-size: 2rem; margin: 0; background: linear-gradient(to right, #6366f1, #a855f7); -webkit-background-clip: text; -webkit-text-fill-color: transparent; }
        
        .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(400px, 1fr)); gap: 24px; }
        
        .card { 
            background: rgba(24, 24, 27, 0.6); 
            backdrop-filter: blur(12px); 
            border: 1px solid var(--border); 
            border-radius: 16px; 
            padding: 24px; 
            transition: transform 0.2s, box-shadow 0.2s;
        }
        .card:hover { transform: translateY(-2px); box-shadow: 0 10px 30px -10px rgba(0,0,0,0.5); border-color: var(--primary); }
        
        h2 { font-size: 1.25rem; margin: 0 0 20px 0; color: var(--text); display: flex; align-items: center; gap: 10px; }
        .status-dot { width: 8px; height: 8px; background: var(--success); border-radius: 50%; box-shadow: 0 0 10px var(--success); }
        
        table { width: 100%; border-collapse: separate; border-spacing: 0; }
        th, td { text-align: left; padding: 12px; border-bottom: 1px solid var(--border); }
        th { color: var(--text-muted); font-weight: 500; font-size: 0.9rem; }
        td { font-size: 0.95rem; }
        tr:last-child td { border-bottom: none; }
        
        .form-row { display: flex; gap: 10px; align-items: center; }
        input { 
            background: var(--surface-highlight); 
            border: 1px solid var(--border); 
            color: white; 
            padding: 10px 14px; 
            border-radius: 8px; 
            outline: none; 
            flex: 1;
            font-family: inherit;
        }
        input:focus { border-color: var(--primary); box-shadow: 0 0 0 2px var(--primary-glow); }
        
        .btn { 
            background: var(--primary); 
            color: white; 
            border: none; 
            padding: 10px 20px; 
            border-radius: 8px; 
            font-weight: 600; 
            cursor: pointer; 
            transition: all 0.2s;
        }
        .btn:hover { filter: brightness(1.1); box-shadow: 0 0 15px var(--primary-glow); }
        .btn-sm { padding: 6px 12px; font-size: 0.85rem; }
        .btn-danger { background: rgba(239, 68, 68, 0.1); color: var(--danger); border: 1px solid var(--danger); }
        .btn-danger:hover { background: var(--danger); color: white; box-shadow: 0 0 15px rgba(239, 68, 68, 0.4); }

        .empty-state { text-align: center; color: var(--text-muted); padding: 20px; font-style: italic; }
        
        .badge { padding: 4px 10px; border-radius: 20px; background: var(--surface-highlight); font-size: 0.8rem; border: 1px solid var(--border); }
        .ping-good { color: var(--success); }
        .ping-bad { color: var(--warning); }
        
        @keyframes pulse { 0% { opacity: 1; } 50% { opacity: 0.5; } 100% { opacity: 1; } }
        .loading { animation: pulse 1.5s infinite; }
    </style>
</head>
<body>
    <div class="container">
        <header>
            <h1>⚡ Tunnel Pro Ultimate</h1>
            <div style="font-size: 0.9rem; color: var(--text-muted);">Max Speed • Latency Monitored</div>
        </header>

        <div class="grid">
            <div class="card">
                <h2><div class="status-dot"></div> Connected Clients</h2>
                <div id="clients-list">
                    <div class="empty-state loading">Loading clients...</div>
                </div>
            </div>

            <div class="card">
                <h2>🚀 Active Tunnels</h2>
                <div id="tunnels-list">
                    <div class="empty-state">No tunnels active</div>
                </div>
            </div>
        </div>
    </div>

    <script>
        function updateStatus() {
            fetch('/api/status')
                .then(r => r.json())
                .then(data => {
                    renderClients(data.clients);
                    renderTunnels(data.listeners);
                });
        }

        function renderClients(clients) {
            const container = document.getElementById('clients-list');
            if (!clients || clients.length === 0) {
                container.innerHTML = '<div class="empty-state">Waiting for connections...</div>';
                return;
            }
            
            let html = '<table><thead><tr><th>ID / IP</th><th>Ping</th><th>Tools</th></tr></thead><tbody>';
            clients.forEach(c => {
                // Check if element exists to preserve input values
                let existingPort = document.getElementById('p-'+c.id);
                let existingTarget = document.getElementById('t-'+c.id);
                let portVal = existingPort ? existingPort.value : '';
                let targetVal = existingTarget ? existingTarget.value : 'localhost:80';

                let pingClass = parseInt(c.latency) > 150 ? 'ping-bad' : 'ping-good';
                html += '<tr>' +
                    '<td><span class="badge">' + c.id + '</span><br><small style="color:var(--text-muted)">' + c.remote_addr + '</small></td>' +
                    '<td class="' + pingClass + '" style="font-weight:bold;">📶 ' + c.latency + '</td>' +
                    '<td><div class="form-row">' +
                    '<input type="number" id="p-'+c.id+'" placeholder="Port" value="'+portVal+'" style="width:60px">' +
                    '<input type="text" id="t-'+c.id+'" placeholder="Target" value="'+targetVal+'" style="width:120px">' +
                    '<button class="btn btn-sm" onclick="openTunnel(\''+c.id+'\')">Open</button>' +
                    '</div></td>' +
                    '</tr>';
            });
            html += '</tbody></table>';
            container.innerHTML = html;
        }

        function renderTunnels(tunnels) {
            const container = document.getElementById('tunnels-list');
             if (!tunnels || tunnels.length === 0) {
                container.innerHTML = '<div class="empty-state">No tunnels active</div>';
                return;
            }
            
            let html = '<table><thead><tr><th>Public Port</th><th>Target</th><th>Action</th></tr></thead><tbody>';
            tunnels.forEach(t => {
                html += '<tr>' +
                    '<td style="color:var(--success); font-weight:bold;">:' + t.port + '</td>' +
                    '<td>' + t.target + '</td>' +
                    '<td><button class="btn btn-sm btn-danger" onclick="closeTunnel(\''+t.port+'\')">Close</button></td>' +
                    '</tr>';
            });
            html += '</tbody></table>';
            container.innerHTML = html;
        }

        function openTunnel(id) {
            const port = document.getElementById('p-'+id).value;
            const target = document.getElementById('t-'+id).value || "localhost:80";
            if(!port) return alert("Enter a port!");
            
            window.location.href = "/open?id=" + id + "&port=" + port + "&target=" + target;
        }

        function closeTunnel(port) {
            if(confirm("Close tunnel on port " + port + "?")) {
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

        let isEditing = false;
        
        document.addEventListener('focusin', function(e) {
            if (e.target.tagName === 'INPUT') isEditing = true;
        });
        
        document.addEventListener('focusout', function(e) {
            if (e.target.tagName === 'INPUT') isEditing = false;
        });

        setInterval(() => {
            if (!isEditing) updateStatus();
        }, 1500);
        
        updateStatus();
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

			// Speed Tune User Connection
			if tcpConn, ok := userConn.(*net.TCPConn); ok {
				tcpConn.SetNoDelay(true)
				tcpConn.SetReadBuffer(4 * 1024 * 1024)
				tcpConn.SetWriteBuffer(4 * 1024 * 1024)
			}

			go func() {
				stream, err := client.session.Open()
				if err != nil {
					userConn.Close()
					return
				}
				stream.Write([]byte(targetAddr + "\n"))

				// Buffered Copy for Speed
				buf1 := make([]byte, 32*1024)
				buf2 := make([]byte, 32*1024)

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
