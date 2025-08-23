# Go SIP Server and Registrar

This project is a SIP server written in Go that acts as a Registrar and a stateful Proxy. It is compliant with RFC3261 for the REGISTER method and can proxy methods like INVITE, ACK, and BYE between registered users. It provides a simple web interface for managing user credentials for Digest Authentication.

The server listens for SIP traffic on UDP and serves a web UI on a separate port.

## Features

- **SIP Registrar**: Handles `REGISTER` requests from SIP clients to bind a user to a location.
- **SIP Proxy**: Forwards requests like `INVITE` and `BYE` between registered users on the same domain. It acts as a stateful proxy by using the `Record-Route` header to remain in the signaling path.
- **Digest Authentication**: Authenticates users using Digest Authentication (MD5) as per RFC2617.
- **Web UI**: A simple web interface to add and view users and their authentication credentials (HA1 hashes).
- **Pure Go**: Built with pure Go libraries, including a pure-Go SQLite driver, making it easy to compile and deploy.

## Dependencies

- **Database**: `github.com/glebarez/go-sqlite` (for SQLite)
- **Concurrency**: `golang.org/x/sync/errgroup`

## Getting Started

### Prerequisites

- Go 1.18 or higher.

### Building

To build the server, clone the repository and run the build command:

```sh
go build -o sip-server ./cmd/server
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

Example:
```sh
./sip-server -web.addr ":9090" -sip.addr ":5061" -sip.realm "my-sip-domain.com"
```

## User Management

Once the server is running, you can access the web UI by navigating to the address specified by `-web.addr` (e.g., `http://localhost:8080`).

From the web UI, you can:
- View a list of all registered users and their HA1 hashes.
- Add new users by providing a username, password, and realm. The server will automatically compute and store the required HA1 hash for authentication.
