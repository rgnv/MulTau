# Plex Aggregator for Tautulli

A Go-based proxy server that aggregates multiple Plex Media Servers into a single unified view for Tautulli monitoring.

## Features

- Aggregates Now Playing sessions from multiple Plex servers
- Combines watch history across all connected Plex servers
- Provides a unified Plex API endpoint for Tautulli
- Handles failover for metadata requests across servers
- Supports secure WebSocket connections to source servers

## Prerequisites

- Go 1.16 or higher
- Access to one or more Plex Media Servers with admin privileges
- Tautulli instance to monitor the aggregated Plex server

## Installation

1. Clone the repository:
   ```bash
   git clone https://github.com/yourusername/tautulli-aggregator.git
   cd tautulli-aggregator
   ```

2. Build the binary:
   ```bash
   go build -o tautulli-aggregator
   ```

## Configuration

Create a `config.json` file in the same directory as the binary with the following structure:

```json
{
  "aggregator_listen_address": "0.0.0.0:32400",
  "source_plex_servers": [
    {
      "name": "Server 1",
      "url": "https://plex1.example.com:32400",
      "token": "your-plex-token-here"
    },
    {
      "name": "Server 2",
      "url": "https://plex2.example.com:32400",
      "token": "your-other-plex-token-here"
    }
  ]
}
```

### Obtaining a Plex Token

1. Sign in to your Plex Web interface
2. Open the developer tools (F12)
3. Go to the Application tab
4. Under Storage > Local Storage, find the `authToken` value

## Running the Aggregator

```bash
./tautulli-aggregator
```

The aggregator will start and listen on the configured address (default: `0.0.0.0:32400`).

## Connecting Tautulli

1. In Tautulli, go to Settings > Plex Media Server
2. Set the following values:
   - Plex URL: `http://<aggregator-ip>:32400`
   - Plex Token: (any non-empty string, as authentication is handled per-server)
3. Save the settings

## How It Works

- The aggregator acts as a reverse proxy for Plex API requests
- It forwards requests to the appropriate source server based on the requested resource
- For metadata requests, it tries each server in sequence until it finds the requested item
- Now Playing sessions are aggregated from all source servers
- Watch history is combined from all connected Plex servers

## API Endpoints

- `/` - Plex server information
- `/status/sessions` - Aggregated Now Playing sessions
- `/status/sessions/history/all` - Combined watch history
- `/library/metadata/{id}` - Proxy to the appropriate Plex server
- `/:/websockets/notifications` - WebSocket notifications endpoint

## Logging

The application logs to stdout with timestamps. You can redirect this to a file for persistent logging:

```bash
./tautulli-aggregator > aggregator.log 2>&1 &
```

## Security Considerations

- The aggregator does not store your Plex tokens in plaintext in memory
- All source server communications use HTTPS when available
- WebSocket connections are secured with WSS when available
- Consider running behind a reverse proxy with HTTPS for production use

## Troubleshooting

### Common Issues

1. **404 errors for metadata**
   - Ensure the item exists on at least one source server
   - Verify the Plex token has sufficient permissions

2. **WebSocket connection issues**
   - Check that the source server URL is accessible from the aggregator
   - Verify the Plex token is valid

3. **Missing Now Playing sessions**
   - Ensure the source server is online and accessible
   - Check the logs for any connection errors

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- Built with Go and the Gorilla WebSocket library
- Inspired by the need for better multi-Plex server monitoring in Tautulli
