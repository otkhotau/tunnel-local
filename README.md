# Tunnel Pro Ultimate - High Performance TCP Tunnel

A robust, high-performance TCP tunneling solution written in Go, featuring a premium Web Dashboard, low-latency monitoring, and Windows Service support.

![Tunnel Pro Dashboard]()

## Features

- **🚀 High Performance**: Tunneled connections use 4MB TCP buffers and 1MB Yamux windows for maximum throughput.
- **🛡️ Resilience**: Automatic reconnection with exponential backoff.
- **🎮 Real-time Dashboard**: 
  - Dark mode "Premium" UI.
  - Live status updates (no refresh needed).
  - Ping/Latency monitoring (<150ms green, >150ms orange).
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

1.  **Connect Client**: Your client will appear in the "Connected Clients" list.
2.  **Open Tunnel**: 
    - Enter a **Public Port** (e.g., `9090`) to open on the Server.
    - Enter a **Target Address** (e.g., `192.168.1.15:80`) reachable by the *Client*.
    - Click **Open**.
3.  **Access**: users can now connect to `SERVER_IP:9090`, and traffic will be tunneled to `192.168.1.15:80` via your client.

## License

MIT License
