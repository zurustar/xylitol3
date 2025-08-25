# Go SIP Server and Registrar

This project is a SIP server written in Go that acts as a Registrar and a stateful Proxy. It is compliant with RFC3261 for the REGISTER method and can proxy methods like INVITE, ACK, and BYE between registered users. It provides a simple web interface for managing user credentials for Digest Authentication.

The server listens for SIP traffic on UDP and TCP, and serves a web UI on a separate port.

## Features

- **SIP Registrar**: Handles `REGISTER` requests from SIP clients to bind a user to a location.
- **SIP Proxy**: Forwards requests like `INVITE` and `BYE` between registered users on the same domain. It acts as a stateful proxy by using the `Record-Route` header to remain in the signaling path.
- **Digest Authentication**: Authenticates users using Digest Authentication (MD5) as per RFC2617.
- **Web UI**: A simple web interface to add and view users and their authentication credentials (HA1 hashes).
- **Session Monitoring**: A web page to view all active SIP calls (dialogs) being handled by the server in real-time.
- **Audio Guidance Service**: Plays a pre-recorded audio announcement when a specific user is called. This feature acts as a simple SIP-to-WebRTC gateway, answering the call and streaming a WAV file to the caller using WebRTC.
- **Pure Go**: Built with pure Go libraries, including a pure-Go SQLite driver, making it easy to compile and deploy.

## Dependencies

- **Database**: `github.com/glebarez/go-sqlite` (for SQLite)
- **WebRTC/Audio**: `github.com/pion/webrtc/v3` (for the Audio Guidance Service)
- **Concurrency**: `golang.org/x/sync/errgroup`

## Getting Started

### Prerequisites

- Go 1.24 or higher.
- A SIP client (e.g., Linphone, Zoiper) for testing.

### Building

To build the server, clone the repository and run the build command:

```sh
go build -o sip-server ./cmd/server/main.go
```

### Running the Server

You can run the server using the compiled binary. It accepts several command-line flags to configure its behavior.

```sh
./sip-server [flags]
```

**Available Flags:**

- `-web.addr`: Address for the web UI server (default `:8080`)
- `-sip.addr`: Address for the SIP server (default `:5060`)
- `-db.path`: Path to the SQLite database file (default `sip_users.db`)
- `-sip.realm`: SIP realm for authentication (default `go-sip-server`)
- `-guidance.user`: A special username that triggers the audio guidance service (default `announcement`)

Example:
```sh
./sip-server -web.addr ":9090" -sip.addr ":5061" -sip.realm "my-sip-domain.com"
```

## User Management and Testing

Once the server is running, you can access the web UI by navigating to the address specified by `-web.addr` (e.g., `http://localhost:8080`).

### 1. Create Users
From the web UI, you can add new users. For example, create two users:
- `user1` with password `pass1`
- `user2` with password `pass2`

### 2. Configure SIP Clients
Configure two SIP clients (or two accounts on one client) to register with the server using the credentials you just created. The domain/proxy address will be the IP and port of your server (e.g., `127.0.0.1:5061`).

### 3. Test a Standard Call
Call from `user1` to `user2`. The server will proxy the call, and the two clients should be able to communicate. You can see the active session on the `/sessions` page of the web UI.

### 4. Test the Audio Guidance Service
From any registered client, make a call to the special guidance user. By default, this is `announcement` (e.g., call `sip:announcement@my-sip-domain.com`).

The server will answer the call and you should hear a pre-recorded audio message. The default message is located at `audio/announcement.wav`. This call also demonstrates the server's WebRTC capabilities.

### 5. Monitor Active Sessions
The web UI provides a page at `/sessions` to see a list of all ongoing calls, including caller, callee, duration, and the SIP Call-ID.
