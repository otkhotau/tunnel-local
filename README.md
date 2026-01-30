# Tunnel Pro Ultimate - High Performance TCP Tunnel

A robust, high-performance TCP tunneling solution written in Go, featuring a premium Web Dashboard, low-latency monitoring, and Windows Service support.

![Tunnel Pro Dashboard](https://raw.githubusercontent.com/otkhotau/tunnel-local/refs/heads/main/tunnel.png)

## Features

- **🚀 High Performance**: 
  - 8MB TCP buffers and 8MB Yamux windows for maximum throughput
  - 128KB copy buffers optimized for 1Gbps+ speeds
  - TCP_NODELAY enabled for minimal latency
- **🛡️ Resilience**: Automatic reconnection with exponential backoff.
- **🎮 Real-time Dashboard**: 
  - Dark mode "Premium" UI
  - Live status updates (no refresh needed)
  - **Client hostname display** - See machine names instead of just IPs
  - **Client version tracking** - Monitor client versions in real-time
  - Ping/Latency monitoring (<100ms green, 100-200ms orange, >200ms red)
- **💾 Smart Port Mapping**: 
  - **Automatic port persistence** - Port mappings are saved per hostname
  - Reconnecting clients automatically get their previous port configuration
  - Saved to `port_mappings.json` on server
- **🔄 Auto-Update System**:
  - **Remote client updates** - Upload new client.exe via web API
  - **Automatic deployment** - Clients auto-check and install updates
  - **Zero-touch updates** - No need to access client machines
  - **Version tracking** - See which clients need updates
- **🔒 Security**: Token-based authentication for clients and Basic Auth for the Dashboard.
- **🔌 Dynamic Port Mapping**: Map any public port on the server to any internal target address on the client side dynamically from the UI.
- **💻 Service Mode**: Run the client as a Windows Service easily.

## Installation

### Prerequisites
- Go 1.20+ installed.

### Build

```bash
# Build Server
go build -o server.exe server.go

# Build Client
go build -o client.exe client.go
```

## Usage

### 1. Server

Start the server on your VPS/Public Machine.

```bash
# Run with defaults (Control: 7000, Web: 8081, Token: secret, User/Pass: admin)
./server.exe

# Custom Configuration
./server.exe -p 9000 -w 8080 -t mysecrettoken -user myuser -pass mypass
```

**Web Dashboard**: Access `http://localhost:8081` (default).

### 2. Client

Connect from your local machine/private network.

```bash
# Run as console application
./client.exe -s "SERVER_IP:7000" -t mysecrettoken

# Run as Windows Service
# 1. Install
./client.exe -service install -s "SERVER_IP:7000" -t mysecrettoken -l "localhost:3389"

# 2. Start
./client.exe -service start

# 3. Uninstall
./client.exe -service stop
./client.exe -service uninstall
```

## Dashboard Guide

1.  **Connect Client**: Your client will appear in the "Connected Clients" list with its **hostname** prominently displayed.
2.  **Open Tunnel**: 
    - If the client has connected before, the **port and target fields will auto-fill** with saved values
    - Enter a **Public Port** (e.g., `9090`) to open on the Server
    - Enter a **Target Address** (e.g., `192.168.1.15:80`) reachable by the *Client*
    - Click **Open**
    - The mapping is automatically saved for next time!
3.  **Access**: users can now connect to `SERVER_IP:9090`, and traffic will be tunneled to `192.168.1.15:80` via your client.

## Performance Optimizations

This version includes several optimizations for 1Gbps+ throughput:

- **8MB TCP buffers** (increased from 4MB)
- **8MB Yamux stream windows** (increased from 1MB)
- **128KB copy buffers** (increased from 32KB)
- TCP_NODELAY enabled on all connections
- Efficient buffered I/O with `io.CopyBuffer`

## Port Mapping Persistence

Port mappings are automatically saved to `port_mappings.json` on the server. The file format:

```json
[
  {
    "hostname": "DESKTOP-ABC123",
    "port": "8080",
    "target": "localhost:80"
  }
]
```

When a client with the same hostname reconnects, the web UI will automatically populate the port and target fields with the saved values.

## Auto-Update System

The tunnel system includes a powerful auto-update feature that allows you to deploy client updates remotely without accessing each machine.

### How It Works

1. **Upload**: Admin uploads new `client.exe` to server via API
2. **Distribution**: Server stores the binary and version info
3. **Auto-Check**: Clients check for updates every 30 minutes
4. **Auto-Install**: Clients automatically download and install updates
5. **Auto-Restart**: Clients restart with the new version

### Uploading an Update

**Using PowerShell Script (Recommended):**
```powershell
.\upload_update.ps1 -Version "2.0.1" -Description "Bug fixes" -ServerIP "192.168.1.100"
```

**Using curl:**
```bash
curl -X POST http://SERVER:8081/api/update/upload \
  -u admin:admin \
  -F "version=2.0.1" \
  -F "description=Bug fixes and improvements" \
  -F "binary=@client.exe"
```

### Client Update Process

- Clients check for updates 10 seconds after startup
- Then check every 30 minutes automatically
- When update found:
  - Download new binary
  - Create self-replacing script
  - Restart with new version
  - Auto-reconnect to server

### Version Tracking

- Server displays client versions in dashboard
- Version info stored in `update_info.json`
- Binaries stored in `updates/` directory
- Check current version: `curl http://SERVER:8081/api/update/download?check=1`

## License

MIT License

