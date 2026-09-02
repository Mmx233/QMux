# QMux

[![Lisense](https://img.shields.io/github/license/Mmx233/QMux)](https://github.com/Mmx233/QMux/blob/main/LICENSE)
[![Test Status](https://img.shields.io/github/actions/workflow/status/Mmx233/QMux/test.yml)](https://github.com/Mmx233/QMux/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/Mmx233/QMux?color=blueviolet&include_prereleases)](https://github.com/Mmx233/QMux/releases)
[![Dockerhub](https://img.shields.io/docker/pulls/mmx233/qmux)](https://hub.docker.com/repository/docker/mmx233/qmux)

A high-availability L4 (transport layer) NAT traversal tool built on the QUIC protocol.

QMux operates at Layer 4 of the OSI model, enabling secure TCP/UDP tunneling through NAT/firewalls by establishing QUIC connections between clients and servers. It works at the transport layer, forwarding raw TCP streams without inspecting application-layer protocols. It supports multi-server configurations with automatic load balancing, mTLS or token authentication, and TLS session resumption for fast reconnections.

# Get Started

## Installation

### From Binary

Download the latest release from [GitHub Releases](https://github.com/Mmx233/QMux/releases).

### From Source

```bash
go install github.com/Mmx233/QMux@latest
```

### Docker

```bash
docker pull mmx233/qmux
```

## Quick Start

### 1. Generate Certificates

QMux uses mTLS for authentication by default. Generate the required certificates for testing:

```bash
qmux generate certs -s your-server-domain.com
```

The `-s` flag specifies the server DNS name for the certificate (required). You can specify multiple names:

```bash
qmux generate certs -s example.com -s qmux.example.com
```

Certificates are valid for 10 years by default. Use `-y` to customize:

```bash
qmux generate certs -s example.com -y 5  # 5 years validity
```

This creates in pwd:
- `certs/ca.crt` - CA certificate
- `certs/server.crt` / `certs/server.key` - Server certificate
- `certs/client.crt` / `certs/client.key` - Client certificate

### 2. Generate Configuration Files

```bash
# Generate server config
qmux generate config server -o server.yaml

# Generate client config
qmux generate config client -o client.yaml
```

### 3. Configure Example Server

Edit `server.yaml`:

```yaml
listeners:
  - quic_addr: "0.0.0.0:8443"    # QUIC control port
    traffic_addr: "0.0.0.0:8080" # Traffic forwarding port
    protocol: "both"              # tcp, udp, or both
    capacity:
      max_client_generations: 16
      max_pending_registrations: 128
      max_tcp_connections: 128
      max_pending_tcp_setups: 128
      max_tcp_connections_per_generation: 100
      max_pending_tcp_setups_per_generation: 16
      max_udp_sessions: 1024
      max_udp_sessions_per_generation: 256

auth:
  method: "mtls"
  ca_cert_file: "./certs/ca.crt"

tls:
  server_cert_file: "./certs/server.crt"
  server_key_file: "./certs/server.key"
```

### 4. Configure Example Client

Edit `client.yaml`:

```yaml
capacity:
  max_local_udp_sessions: 256

server:
  servers:
    - address: "your-server-ip:8443"
      server_name: "qmux-server"

local:
  host: "127.0.0.1"
  port: 3000  # Your local service port

auth:
  method: "mtls"

tls:
  ca_cert_file: "./certs/ca.crt"
  client_cert_file: "./certs/client.crt"
  client_key_file: "./certs/client.key"
```

| Key                                     | Scope                                                                  | Default | When full                                                                 |
|-----------------------------------------|------------------------------------------------------------------------|--------:|---------------------------------------------------------------------------|
| `max_client_generations`                | Server listener: client generations                                    |      16 | Reject the new client registration.                                       |
| `max_pending_registrations`             | Server listener: registrations not yet committed                       |     128 | Close the new registration connection.                                    |
| `max_tcp_connections`                   | Server listener: pending and active TCP flows                          |     128 | Close the new TCP connection.                                             |
| `max_pending_tcp_setups`                | Server listener: TCP flows still setting up                            |     128 | Close the new TCP connection.                                             |
| `max_tcp_connections_per_generation`    | Server listener: pending and active TCP flows on one client generation |     100 | Try another eligible generation; otherwise close the new TCP connection.  |
| `max_pending_tcp_setups_per_generation` | Server listener: pending TCP setups on one client generation           |      16 | Try another eligible generation; otherwise close the new TCP connection.  |
| `max_udp_sessions`                      | Server listener: server-side UDP sessions                              |    1024 | Drop the datagram that would create a new session.                        |
| `max_udp_sessions_per_generation`       | Server listener: server-side UDP sessions on one client generation     |     256 | Try another eligible generation; otherwise drop the new-session datagram. |
| `max_local_udp_sessions`                | Client process: local UDP sessions shared by all UDP handlers          |     256 | Drop the datagram that would create a new local session.                  |

All eight server settings are enforced independently for each `listeners` entry. `max_local_udp_sessions` is one process-wide client budget. Omitting a setting or using `0` selects its default; negative values are rejected, and there is no unlimited setting. When a gate is full, QMux rejects or drops only new work; it does not evict registered generations, established TCP connections, or existing UDP sessions.

The per-generation TCP gates are independent: `max_tcp_connections_per_generation` (100 by default) counts pending and active connections, while `max_pending_tcp_setups_per_generation` (16 by default) counts only connections still being established. Completing setup releases the pending-setup slot but continues to consume a connection slot until the connection closes.

For multi-listener deployments, calculate the aggregate budget from the actual listener count and workload, then validate the limits under that topology.

### 5. Run

```bash
# Start server (on public server)
qmux run server -c server.yaml

# Optionally expose liveness/readiness on loopback
qmux run server -c server.yaml --admin-address 127.0.0.1:9090

# Start client (on machine behind NAT)
qmux run client -c client.yaml
```

Now external traffic to `your-server-ip:8080` will be forwarded to your local service on port 3000.

The admin listener is disabled by default and serves `GET /healthyz` and `GET /readyz` when enabled. It has no authentication, so bind it only to loopback or a protected management network. Readiness returns `200 ok` only when every configured route is listening and has an eligible client for each enabled protocol; otherwise it returns `503 not ready`. QMux only reports this state. Any load-balancer or scheduler action remains external, and a business-port blackbox check remains the end-to-end data-path test.

## Authentication

QMux uses mTLS by default. In this mode the server and client verify one another with certificates, as shown in the quick-start configuration above.

Token authentication is available when distributing a client certificate is undesirable. Generate a machine-random secret with at least 256 bits of entropy, for example with `openssl rand -base64 32`, and configure the same value on both sides:

```yaml
# server.yaml
auth:
  method: "token"
  token: "replace-with-the-generated-secret"

tls:
  server_cert_file: "./certs/server.crt"
  server_key_file: "./certs/server.key"
```

```yaml
# client.yaml
auth:
  method: "token"
  token: "replace-with-the-generated-secret"

tls:
  ca_cert_file: "./certs/ca.crt"
```

The token itself is never sent over the wire. After the TLS 1.3 handshake completes, the client sends an HMAC-SHA512 registration proof bound to that connection through the TLS exporter. The client still validates the server certificate using `ca_cert_file`, while token mode neither requires nor sends a client certificate. A proof captured from one connection cannot authenticate another connection, including a resumed TLS session.

### Session ticket key rotation

By default, QMux leaves session ticket key rotation to Go. In the Go 1.27 baseline, Go rotates keys every 24 hours and expires them after 7 days; Go owns this automatic policy.

Set `tls.session_ticket_encryption_key_rotation_interval` to a positive duration to use QMux's custom rotation. `tls.session_ticket_encryption_key_rotation_overlap` is the number of old keys retained: omission or `null` uses the fixed default of 7 old keys (8 total), while numeric `0` retains no old keys. The overlap setting is invalid when the interval is zero or omitted.

# Performance

## Test Environment

| Component | Specification        |
|-----------|----------------------|
| CPU       | Apple M4             |
| Cores     | 10                   |
| Memory    | 16 GB                |
| OS        | macOS (darwin/arm64) |
| Go        | 1.25.5               |

## Benchmark Results

### Discard Test

| Test | Raw (Baseline) | QMux        | Efficiency |
|------|----------------|-------------|------------|
| TCP  | 126,410 Mbps   | 2,710 Mbps  | 2.1%       |
| UDP  | 6,081 Mbps     | 3,430 Mbps  | 56.4%      |

### iperf3 Comprehensive Benchmark

#### TCP

| Test                  | Throughput (recv) |
|-----------------------|-------------------|
| TCP Baseline 1-thread | 129,551 Mbps      |
| TCP Baseline 2-thread | 151,449 Mbps      |
| TCP Baseline 4-thread | 114,702 Mbps      |
| TCP QMux 1-thread     | 2,773 Mbps        |
| TCP QMux 2-thread     | 2,574 Mbps        |
| TCP QMux 4-thread     | 2,304 Mbps        |

#### UDP

| Test                  | Throughput (recv) |
|-----------------------|-------------------|
| UDP Baseline 1-thread | 25,309 Mbps       |
| UDP Baseline 2-thread | 45,435 Mbps       |
| UDP QMux 1-thread     | 1,343 Mbps        |
| UDP QMux 2-thread     | 1,122 Mbps        |


Notes:

- Raw baseline represents direct loopback performance without any tunneling
- QMux adds QUIC encryption, multiplexing, and protocol overhead
- All tests use only 1 QUIC connection
- Process CPU and RSS figures are intentionally not published until the separate-process sampler has calibrated five-run evidence

Run benchmarks yourself:

```bash
# Quick speed report
go test -v -run '^TestSpeedReport$' ./e2e

# Comprehensive iperf3 benchmark
# Require iperf3 installed
go test -v -run '^TestIperf3' ./e2e
```

# High Available Architecture

## System Overview

```mermaid
flowchart TD
    subgraph External["External Traffic"]
        direction LR
        User1[Users :80]
        User2[Users :443]
    end

    subgraph LBLayer["Load Balancers"]
        direction LR
        LB1[LB :80]
        LB2[LB :443]
    end

    subgraph ServerCluster["QMux Server Cluster"]
        direction LR
        S1[Server 1<br/>QUIC:8443,8444<br/>Traffic:80,443]
        S2[Server 2<br/>QUIC:8443,8444<br/>Traffic:80,443]
    end

    subgraph ClientLayer["QMux Client Pods"]
        direction LR
        subgraph ClientGroup1["Client Group A (Port 80)"]
            C1A[Client Pod 1]
            C1B[Client Pod 2]
        end
        subgraph ClientGroup2["Client Group B (Port 443)"]
            C2A[Client Pod 1]
            C2B[Client Pod 2]
        end
    end

    subgraph Services["Internal Services"]
        direction LR
        SVC1[Service A<br/>:3000]
        SVC2[Service B<br/>:5000]
    end

    User1 --> LB1
    User2 --> LB2
    LB1 --> S1 & S2
    LB2 --> S1 & S2

    S1 & S2 <-.->|QUIC :8443| C1A & C1B
    S1 & S2 <-.->|QUIC :8444| C2A & C2B

    C1A & C1B --> SVC1
    C2A & C2B --> SVC2
```

## Data Tunnel Flow

```mermaid
sequenceDiagram
    participant User as External User
    participant LB as L4 Load Balancer
    participant Server as QMux Server
    participant Pool as Connection Pool
    participant Client as K8s Client Pod
    participant SVC as K8s Service

    User->>LB: TCP Connection
    LB->>Server: Forward to healthy server
    Server->>Pool: Select() - Round Robin / Least Connection
    Pool-->>Server: ClientConn
    
    Server->>Client: OpenStream() via existing QUIC conn
    Server->>Client: NewConnMsg{ConnID, Protocol, SourceAddr}
    
    Client->>SVC: Dial internal service
    SVC-->>Client: Connected
    
    Note over User,SVC: Bidirectional Tunnel Established
    
    par Data Flow
        User->>LB: Request Data
        LB->>Server: Forward
        Server->>Client: Stream Data
        Client->>SVC: Forward
    and
        SVC->>Client: Response
        Client->>Server: Stream Data
        Server->>LB: Forward
        LB->>User: Response
    end
```
