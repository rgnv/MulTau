# Plex Aggregator for Tautulli

A Go-based proxy server that aggregates multiple Plex Media Servers into a single unified view, primarily for Tautulli monitoring. It can also serve as a general-purpose Plex aggregator for other tools that expect a single Plex server endpoint.

## Features

-   **Unified Plex API:** Presents multiple Plex servers as a single instance.
-   **Aggregated "Now Playing":** Combines `/status/sessions` from all configured Plex servers.
-   **Aggregated Watch History:** Merges `/status/sessions/history/all` from all servers, sorts, and deduplicates entries.
-   **WebSocket Aggregation:** Relays WebSocket notifications from all source servers to connected clients (e.g., Tautulli).
-   **Metadata Failover:** Forwards metadata requests (`/library/metadata/...`) to source servers; specific server targeting is not implemented (it proxies to the first available or relevant server based on internal logic).
-   **Optional Authentication:** Secure the aggregator endpoint with a configurable `X-Plex-Token`.
-   **Docker Support:** Includes a `Dockerfile` for easy containerized deployment.
-   **Configurable Sync Interval:** Periodically fetches and updates the aggregated watch history.

## Prerequisites

### For Running from Source:

-   Go 1.16 or higher.
-   Access to one or more Plex Media Servers with admin privileges (for obtaining tokens and for the aggregator to connect).
-   Tautulli instance (or other client) to connect to the aggregator.

### For Running with Docker:

-   Docker installed and running.

## Installation

### 1. From Source

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/yourusername/MulTau.git 
    cd MulTau
    ```
    *(Replace `yourusername/MulTau` with the actual repository URL)*

2.  **Build the binary:**
    ```bash
    go build -o MulTau main.go
    ```
    *(The binary will be named `MulTau`)*

### 2. Using Docker

1.  **Prepare `config.json`:** Create your `config.json` file as described in the [Configuration](#configuration) section.
2.  **Build the image:**
    Navigate to the project's root directory (where the `Dockerfile` is located) and run:
    ```bash
    docker build -t MulTau-image .
    ```
    *(You can choose a different tag like `yourusername/MulTau:latest`)*

## Configuration

Create a `config.json` file in the same directory as the binary (if running from source) or in a location accessible for mounting into the Docker container.

```json
{
  "aggregator_listen_address": "0.0.0.0:32400",
  "source_plex_servers": [
    {
      "name": "Plex Server 1 (Main)",
      "url": "https://plex1.example.com:32400",
      "plex_token": "YOUR_PLEX_TOKEN_FOR_SERVER1"
    },
    {
      "name": "Plex Server 2 (Archive)",
      "url": "http://plex2.local:32400",
      "plex_token": "YOUR_PLEX_TOKEN_FOR_SERVER2"
    }
  ],
  "aggregator_plex_token": "YOUR_SECRET_AGGREGATOR_TOKEN",
  "sync_interval_minutes": 30,
  "log_level": "info"
}
```

**Configuration Options:**

-   `aggregator_listen_address` (string, required): The IP address and port for the aggregator to listen on (e.g., `0.0.0.0:32400` to listen on all interfaces, port 32400).
-   `source_plex_servers` (array of objects, required): A list of your Plex servers to aggregate.
    -   `name` (string, optional): A friendly name for the server (used in logs). If omitted, a default name like "Server 1" will be used.
    -   `url` (string, required): The full URL of the Plex server (e.g., `https://your-plex-domain.com:32400` or `http://local-ip:32400`).
    -   `plex_token` (string, required): The `X-Plex-Token` for this specific Plex server.
-   `aggregator_plex_token` (string, optional): If set, clients (like Tautulli) must provide this token in the `X-Plex-Token` header (or query parameter for WebSockets) to connect to the aggregator. If empty or omitted, no authentication is enforced by the aggregator.
-   `sync_interval_minutes` (integer, optional, default: 0 or disabled if not set): How often (in minutes) the aggregator should re-fetch and merge watch history from all source servers. If set to 0 or omitted, history sync will run once at startup and then stop.
-   `log_level` (string, optional): **Currently not implemented.** This setting is defined but does not yet control the application's log verbosity. Logging is currently at a fixed level.

### Obtaining a Plex Token (`plex_token` for source servers)

1.  Sign in to your Plex Web interface for the target server.
2.  Open your browser's developer tools (usually F12).
3.  Navigate to the "Application" (or "Storage") tab.
4.  Under "Local Storage", find the entry for your Plex server's URL.
5.  Locate the `authToken` value. This is your `X-Plex-Token`.

## Running the Aggregator

### 1. From Source

Ensure your `config.json` is in the same directory as the `MulTau` binary.

```bash
./MulTau
```

The aggregator will start and log its activity to standard output.

### 2. Using Docker

To run the aggregator in a Docker container:

-   Map the port specified in `aggregator_listen_address`.
-   Mount your `config.json` file into the container at `/app/config.json`.

```bash
docker run -d \
  --name MulTau-container \
  -p 32400:32400 \
  -v /path/to/your/config.json:/app/config.json \
  MulTau-image
```

-   Replace `/path/to/your/config.json` with the actual absolute path to your `config.json` file on your host.
-   Ensure the host port in `-p 32400:32400` matches the port in your `aggregator_listen_address`.
-   `MulTau-image` should be the tag you used when building the Docker image.

**Note on `config.json` with Docker:**

The `Dockerfile` copies a `config.json` if present in the build context. However, **it is strongly recommended to mount your specific `config.json` at runtime** as shown above. This ensures your sensitive data and specific configurations are used without rebuilding the image and keeps your tokens out of the image layers.

### Managing the Docker Container

-   **View logs:**
    ```bash
    docker logs MulTau-container
    ```
-   **Stop the container:**
    ```bash
    docker stop MulTau-container
    ```
-   **Start the container:**
    ```bash
    docker start MulTau-container
    ```
-   **Remove the container (after stopping):**
    ```bash
    docker rm MulTau-container
    ```

## Connecting Tautulli

1.  In Tautulli, go to **Settings > Plex Media Server**.
2.  Set the **Plex URL** to the aggregator's address (e.g., `http://<aggregator-ip-or-hostname>:<port>`).
    -   If running Docker on the same machine as Tautulli, this might be `http://localhost:32400` or `http://<docker_host_ip>:32400`.
    -   If running on a different machine, use that machine's IP or hostname.
3.  Set the **Plex Token**:
    -   If you have set `aggregator_plex_token` in your `config.json`, use that token here.
    -   If `aggregator_plex_token` is not set (or is empty), Tautulli still requires a token. You can enter any non-empty string (e.g., `dummy_token` or `not_used`). The aggregator will ignore it, but Tautulli needs something in this field.
4.  Save the settings. Tautulli should now connect to the aggregator and see a unified view of your Plex servers.

## API Endpoints

The aggregator proxies most Plex API endpoints. Key aggregated endpoints include:

-   `/`: Basic Plex server information (identifies as the aggregator).
-   `/status/sessions`: Aggregated "Now Playing" sessions.
-   `/status/sessions/history/all`: Aggregated watch history.
-   `/:/websockets/notifications` (and `/ws`): WebSocket endpoint for aggregated real-time notifications.
-   `/library/metadata/{id}`: Proxies to source servers for metadata.

## How It Works

-   **Proxying:** Acts as a reverse proxy for most Plex API requests.
-   **Aggregation:**
    -   For `/status/sessions`, it fetches data from all source servers and merges the results.
    -   For `/status/sessions/history/all`, it fetches, merges, sorts by `viewedAt` (descending), and deduplicates history items.
    -   WebSocket notifications from all source servers are forwarded to clients connected to the aggregator's WebSocket endpoint.
-   **Authentication:**
    -   Uses the `plex_token` from `config.json` to authenticate with each source Plex server.
    -   Optionally uses `aggregator_plex_token` to authenticate clients connecting to the aggregator.

## Logging

The application logs to standard output. You can redirect this to a file for persistent logging:

```bash
./MulTau > aggregator.log 2>&1 &
```

## Security Considerations

-   **Plex Tokens:** Your source Plex server tokens (`plex_token`) are stored in `config.json`. Protect this file.
-   **Aggregator Token:** If you use `aggregator_plex_token`, ensure it's a strong, unique token.
-   **Network Exposure:** The aggregator listens on the configured `aggregator_listen_address`. If this is exposed to the internet (e.g., `0.0.0.0` on a public server), it's highly recommended to:
    -   Use a strong `aggregator_plex_token`.
    -   Run the aggregator behind a reverse proxy (like Nginx or Caddy) with HTTPS enabled.
-   **HTTPS:** The aggregator itself serves HTTP. For HTTPS, use a reverse proxy.

## Troubleshooting

-   **Check Aggregator Logs:** The first place to look for issues. If running via Docker, use `docker logs MulTau-container`.
-   **401 Unauthorized (from Aggregator):** If you've set `aggregator_plex_token`, ensure your client (Tautulli) is configured with the correct token.
-   **401 Unauthorized (in logs, related to source servers):** Verify the `plex_token` for each server in `config.json` is correct and has not expired.
-   **Connection Issues to Source Servers:** Ensure the `url` for each source server is correct and reachable from where the aggregator is running. Check for firewall issues.
-   **No Data in Tautulli:** 
    -   Verify Tautulli is connected to the aggregator (check Tautulli logs).
    -   Check aggregator logs for errors fetching data from source servers.
-   **Incorrect History/Sessions:** Ensure `sync_interval_minutes` is set appropriately if you expect frequent updates to history.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

-   Built with Go, Gorilla Mux, and Gorilla WebSocket.
-   Inspired by the need for unified multi-Plex server monitoring in Tautulli.
