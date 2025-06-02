// Copyright (c) 2025 rgnv <rgnv@proton.me>
// MulTau - A Plex Aggregator
//
// This program acts as a reverse proxy to aggregate multiple Plex Media Servers
// into a unified view, primarily for Tautulli monitoring.

package main

import (
	// Standard library imports
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	// Third-party imports
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

type PlexInstanceConfig struct {
	Name      string `json:"name,omitempty"` // Optional friendly name
	URL       string `json:"url"`
	PlexToken string `json:"plex_token"`
}

type Config struct {
	SourcePlexServers       []PlexInstanceConfig `json:"source_plex_servers"`
	AggregatorListenAddress string               `json:"aggregator_listen_address"`
	ListenAddress           string               `json:"-"` // Computed from AggregatorListenAddress
	SyncIntervalMinutes     int                  `json:"sync_interval_minutes"` // Retained for potential periodic history fetching from source Plex servers
	LogLevel                string               `json:"log_level,omitempty"`
	AggregatorPlexToken     string               `json:"aggregator_plex_token,omitempty"` // Token clients must use to connect to this aggregator
	MachineIdentifier       string               `json:"machine_identifier,omitempty"`
}

// PlexHistoryItem defines the structure for individual history entries from Plex.
// Attributes are based on common fields found in the /status/sessions/history/all XML response.
type PlexHistoryItem struct {
	Key                   string `xml:"key,attr,omitempty"`
	RatingKey             string `xml:"ratingKey,attr,omitempty"`
	ParentRatingKey       string `xml:"parentRatingKey,attr,omitempty"`
	GrandparentRatingKey  string `xml:"grandparentRatingKey,attr,omitempty"`
	Title                 string `xml:"title,attr,omitempty"`
	GrandparentTitle      string `xml:"grandparentTitle,attr,omitempty"` // e.g., Show title
	ParentTitle           string `xml:"parentTitle,attr,omitempty"`      // e.g., Season title
	Type                  string `xml:"type,attr,omitempty"`             // e.g., movie, episode
	Thumb                 string `xml:"thumb,attr,omitempty"`
	Art                   string `xml:"art,attr,omitempty"`
	ParentThumb           string `xml:"parentThumb,attr,omitempty"`
	GrandparentThumb      string `xml:"grandparentThumb,attr,omitempty"`
	OriginallyAvailableAt string `xml:"originallyAvailableAt,attr,omitempty"` // Date string
	ViewedAt              int64  `xml:"viewedAt,attr,omitempty"`              // Unix timestamp
	AccountID             int    `xml:"accountID,attr,omitempty"`
	DeviceID              int    `xml:"deviceID,attr,omitempty"`
	SourceServerName      string `xml:"-"` // Internal: Not part of Plex XML, used to track origin
}

// PlexHistoryMediaContainer is the root element for the Plex history XML response.
type PlexHistoryMediaContainer struct {
	XMLName           xml.Name          `xml:"MediaContainer"`
	Size              int               `xml:"size,attr"`
	AllowSync         bool              `xml:"allowSync,attr,omitempty"`
	Identifier        string            `xml:"identifier,attr,omitempty"`
	MediaTagPrefix    string            `xml:"mediaTagPrefix,attr,omitempty"`
	MediaTagVersion   int64             `xml:"mediaTagVersion,attr,omitempty"`
	LibrarySectionID  int               `xml:"librarySectionID,attr,omitempty"`
	LibrarySectionUUID string           `xml:"librarySectionUUID,attr,omitempty"`
	Videos            []PlexHistoryItem `xml:"Video"`
}

// JSONPlexHistoryItem defines the structure for individual history entries from Plex JSON response.
type JSONPlexHistoryItem struct {
	Key                   string `json:"key,omitempty"`
	RatingKey             string `json:"ratingKey,omitempty"`
	ParentRatingKey       string `json:"parentRatingKey,omitempty"`
	GrandparentRatingKey  string `json:"grandparentRatingKey,omitempty"`
	Title                 string `json:"title,omitempty"`
	GrandparentTitle      string `json:"grandparentTitle,omitempty"`
	ParentTitle           string `json:"parentTitle,omitempty"`
	Type                  string `json:"type,omitempty"`
	Thumb                 string `json:"thumb,omitempty"`
	Art                   string `json:"art,omitempty"`
	ParentThumb           string `json:"parentThumb,omitempty"`
	GrandparentThumb      string `json:"grandparentThumb,omitempty"`
	OriginallyAvailableAt string `json:"originallyAvailableAt,omitempty"`
	ViewedAt              int64  `json:"viewedAt,omitempty"`
	AccountID             int    `json:"accountID,omitempty"`
	DeviceID              int    `json:"deviceID,omitempty"`
	// Fields like historyKey are in JSON but not directly used in our common PlexHistoryItem yet.
}

// JSONPlexMediaContainer holds the top-level structure for JSON history responses.
type JSONPlexMediaContainer struct {
	MediaContainer struct {
		Size     int                   `json:"size"`
		Metadata []JSONPlexHistoryItem `json:"Metadata"`
	} `json:"MediaContainer"`
}

// PlexSessionUser represents a user in a session
type PlexSessionUser struct {
	ID    string `xml:"id,attr,omitempty"`
	Thumb string `xml:"thumb,attr,omitempty"`
	Title string `xml:"title,attr,omitempty"`
}

// PlexSessionPlayer represents a player in a session
type PlexSessionPlayer struct {
	Address            string `xml:"address,attr,omitempty"`
	Device             string `xml:"device,attr,omitempty"`
	MachineIdentifier  string `xml:"machineIdentifier,attr,omitempty"`
	Model              string `xml:"model,attr,omitempty"`
	Platform           string `xml:"platform,attr,omitempty"`
	PlatformVersion    string `xml:"platformVersion,attr,omitempty"`
	Product            string `xml:"product,attr,omitempty"`
	Profile            string `xml:"profile,attr,omitempty"`
	State              string `xml:"state,attr,omitempty"`
	Title              string `xml:"title,attr,omitempty"`
	Version            string `xml:"version,attr,omitempty"`
	Local              string `xml:"local,attr,omitempty"`
	Relayed            string `xml:"relayed,attr,omitempty"`
	Secure             string `xml:"secure,attr,omitempty"`
	UserID             string `xml:"userID,attr,omitempty"`
}

// PlexSessionInfo represents session information
type PlexSessionInfo struct {
	ID        string `xml:"id,attr,omitempty"`
	Bandwidth string `xml:"bandwidth,attr,omitempty"`
	Location  string `xml:"location,attr,omitempty"`
}

// PlexTranscodeSession represents a transcode session
type PlexTranscodeSession struct {
	Key                    string `xml:"key,attr,omitempty"`
	Throttled              string `xml:"throttled,attr,omitempty"`
	Complete               string `xml:"complete,attr,omitempty"`
	Progress               string `xml:"progress,attr,omitempty"`
	Size                   string `xml:"size,attr,omitempty"`
	Speed                  string `xml:"speed,attr,omitempty"`
	Error                  string `xml:"error,attr,omitempty"`
	Duration               string `xml:"duration,attr,omitempty"`
	Remaining              string `xml:"remaining,attr,omitempty"`
	Context                string `xml:"context,attr,omitempty"`
	SourceVideoCodec       string `xml:"sourceVideoCodec,attr,omitempty"`
	SourceAudioCodec       string `xml:"sourceAudioCodec,attr,omitempty"`
	VideoDecision          string `xml:"videoDecision,attr,omitempty"`
	AudioDecision          string `xml:"audioDecision,attr,omitempty"`
	Protocol               string `xml:"protocol,attr,omitempty"`
	Container              string `xml:"container,attr,omitempty"`
	VideoCodec             string `xml:"videoCodec,attr,omitempty"`
	AudioCodec             string `xml:"audioCodec,attr,omitempty"`
	AudioChannels          string `xml:"audioChannels,attr,omitempty"`
	TranscodeHwRequested   string `xml:"transcodeHwRequested,attr,omitempty"`
	TranscodeHwEncoding    string `xml:"transcodeHwEncoding,attr,omitempty"`
	TranscodeHwEncodingTitle string `xml:"transcodeHwEncodingTitle,attr,omitempty"`
	TimeStamp              string `xml:"timeStamp,attr,omitempty"`
	MaxOffsetAvailable     string `xml:"maxOffsetAvailable,attr,omitempty"`
	MinOffsetAvailable     string `xml:"minOffsetAvailable,attr,omitempty"`
}

// PlexSessionItem represents a session item
type PlexSessionItem struct {
	XMLName               xml.Name              `xml:""`
	AddedAt               string                `xml:"addedAt,attr,omitempty"`
	Art                   string                `xml:"art,attr,omitempty"`
	CreatedAtAccuracy     string                `xml:"createdAtAccuracy,attr,omitempty"`
	CreatedAtTZOffset     string                `xml:"createdAtTZOffset,attr,omitempty"`
	Duration              string                `xml:"duration,attr,omitempty"`
	GrandparentArt        string                `xml:"grandparentArt,attr,omitempty"`
	GrandparentGuid       string                `xml:"grandparentGuid,attr,omitempty"`
	GrandparentKey        string                `xml:"grandparentKey,attr,omitempty"`
	GrandparentRatingKey  string                `xml:"grandparentRatingKey,attr,omitempty"`
	GrandparentThumb      string                `xml:"grandparentThumb,attr,omitempty"`
	GrandparentTitle      string                `xml:"grandparentTitle,attr,omitempty"`
	Guid                  string                `xml:"guid,attr,omitempty"`
	Index                 string                `xml:"index,attr,omitempty"`
	Key                   string                `xml:"key,attr,omitempty"`
	LastViewedAt          string                `xml:"lastViewedAt,attr,omitempty"`
	LibrarySectionID      string                `xml:"librarySectionID,attr,omitempty"`
	LibrarySectionKey     string                `xml:"librarySectionKey,attr,omitempty"`
	LibrarySectionTitle   string                `xml:"librarySectionTitle,attr,omitempty"`
	OriginallyAvailableAt string                `xml:"originallyAvailableAt,attr,omitempty"`
	ParentGuid            string                `xml:"parentGuid,attr,omitempty"`
	ParentIndex           string                `xml:"parentIndex,attr,omitempty"`
	ParentKey             string                `xml:"parentKey,attr,omitempty"`
	ParentRatingKey       string                `xml:"parentRatingKey,attr,omitempty"`
	ParentStudio          string                `xml:"parentStudio,attr,omitempty"`
	ParentThumb           string                `xml:"parentThumb,attr,omitempty"`
	ParentTitle           string                `xml:"parentTitle,attr,omitempty"`
	ParentYear            string                `xml:"parentYear,attr,omitempty"`
	RatingCount           string                `xml:"ratingCount,attr,omitempty"`
	RatingKey             string                `xml:"ratingKey,attr,omitempty"`
	SessionKey            string                `xml:"sessionKey,attr,omitempty"`
	SkipCount             string                `xml:"skipCount,attr,omitempty"`
	Subtype               string                `xml:"subtype,attr,omitempty"`
	Thumb                 string                `xml:"thumb,attr,omitempty"`
	Title                 string                `xml:"title,attr,omitempty"`
	Type                  string                `xml:"type,attr,omitempty"`
	UpdatedAt             string                `xml:"updatedAt,attr,omitempty"`
	ViewCount             string                `xml:"viewCount,attr,omitempty"`
	ViewOffset            string                `xml:"viewOffset,attr,omitempty"`
	Year                  string                `xml:"year,attr,omitempty"`
	
	// Inner elements - using pointers to make them optional
	User             *PlexSessionUser      `xml:"User,omitempty"`
	Player           *PlexSessionPlayer    `xml:"Player,omitempty"`
	Session          *PlexSessionInfo      `xml:"Session,omitempty"`
	TranscodeSession *PlexTranscodeSession `xml:"TranscodeSession,omitempty"`
	
	// Raw inner XML for Media, Mood, and other nested elements
	InnerXML         string                `xml:",innerxml"`
}

// PlexSessionsMediaContainer represents a container for session items
type PlexSessionsMediaContainer struct {
	XMLName xml.Name          `xml:"MediaContainer"`
	Size    int               `xml:"size,attr"`
	Items   []PlexSessionItem `xml:",any"`
}

var (
	wsServer           *WebSocketServer
	config             *Config
	mergedHistory      []PlexHistoryItem
	mergedHistoryMutex sync.RWMutex
) // Restored wsServer declaration

// HTTP client pools for better performance
var (
	// Default HTTP client with TLS verification
	defaultHTTPClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// HTTP client with insecure TLS (for self-signed certificates)
	insecureHTTPClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Proxy HTTP client with longer timeout and no compression
	proxyHTTPClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			DisableCompression:  true,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
)

// Buffer pool for response body reading
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// WebSocketIdentity structures for the initial message to the client
type WebSocketIdentityServerInfo struct {
	FriendlyName      string `json:"friendlyName"`
	MachineIdentifier string `json:"machineIdentifier"`
}

type WebSocketIdentityContainer struct {
	Size   int                         `json:"size"`
	Type   string                      `json:"type"`
	Server WebSocketIdentityServerInfo `json:"Server,omitempty"`
}

type WebSocketIdentityNotification struct {
	NotificationContainer WebSocketIdentityContainer `json:"NotificationContainer"`
}

type PlexEvent struct {
	EventType string `json:"event_type"`
	Session   struct {
		RatingKey         string `json:"rating_key"`
		Player            string `json:"player"`
		SessionKey        string `json:"session_key"`
		User              string `json:"user"`
		State             string `json:"state"`
		Duration          int    `json:"duration"`
		ViewOffset        int    `json:"view_offset"`
		TranscodeDecision string `json:"transcode_decision"`
	} `json:"session"`
	SourceServer string `json:"source_server"` // Added to track which server this event came from
}

// checkAuth verifies the X-Plex-Token if appConfig.AggregatorPlexToken is set.
// For WebSockets, token is expected in query param. For HTTP, in header.
func checkAuth(r *http.Request, isWebSocket bool) bool {
	if config.AggregatorPlexToken == "" {
		return true // No token configured, authentication skipped
	}

	clientToken := ""
	if isWebSocket {
		clientToken = r.URL.Query().Get("X-Plex-Token")
	} else {
		clientToken = r.Header.Get("X-Plex-Token")
	}

	if clientToken == "" {
		log.Printf("Auth failed: X-Plex-Token missing from client %s for %s", r.RemoteAddr, r.URL.Path)
		return false
	}

	if clientToken != config.AggregatorPlexToken {
		log.Printf("Auth failed: Invalid X-Plex-Token from client %s for %s", r.RemoteAddr, r.URL.Path)
		return false
	}

	return true
}

type WebSocketClient struct {
	conn   *websocket.Conn
	send   chan []byte
	remote string
}

type WebSocketServer struct {
	clients    map[*WebSocketClient]bool
	broadcast  chan []byte
	register   chan *WebSocketClient
	unregister chan *WebSocketClient
	mu         sync.Mutex
}

func startWebSocketServer(ctx context.Context, wg *sync.WaitGroup) *WebSocketServer {
	defer wg.Done()
	
	server := &WebSocketServer{
		broadcast:  make(chan []byte, 256), // Buffered channel for better performance
		register:   make(chan *WebSocketClient),
		unregister: make(chan *WebSocketClient),
		clients:    make(map[*WebSocketClient]bool),
	}

	go server.run(ctx)
	return server
}

func (server *WebSocketServer) run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: close all client connections
			for client := range server.clients {
				close(client.send)
				delete(server.clients, client)
			}
			return
			
		case client := <-server.register:
			server.clients[client] = true
			log.Printf("WebSocket client registered. Total clients: %d", len(server.clients))

		case client := <-server.unregister:
			if _, ok := server.clients[client]; ok {
				delete(server.clients, client)
				close(client.send)
				log.Printf("WebSocket client unregistered. Total clients: %d", len(server.clients))
			}

		case message := <-server.broadcast:
			// Send to all clients with non-blocking writes
			for client := range server.clients {
				select {
				case client.send <- message:
				default:
					// Client's send channel is full, close it
					log.Printf("WebSocket client send buffer full, closing connection")
					delete(server.clients, client)
					close(client.send)
				}
			}
			
		case <-ticker.C:
			// Periodic cleanup and stats
			log.Printf("WebSocket server stats: %d connected clients, broadcast queue: %d/%d", 
				len(server.clients), len(server.broadcast), cap(server.broadcast))
		}
	}
}

func loadConfig(configPath string) *Config {
	file, err := os.Open(configPath)
	if err != nil {
		log.Fatalf("Failed to open config file: %v", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		log.Fatalf("Failed to decode config: %v", err)
	}

	// Generate Machine Identifier from config or use provided one
	if cfg.MachineIdentifier == "" {
		cfg.MachineIdentifier = generateMachineIdentifier(cfg)
	}

	// Set ListenAddress from AggregatorListenAddress for backward compatibility
	cfg.ListenAddress = cfg.AggregatorListenAddress

	return &cfg
}

func generateMachineIdentifier(cfg Config) string {
	hash := sha256.Sum256([]byte(cfg.MachineIdentifier))
	return hex.EncodeToString(hash[:])
}

func main() {
	var wg sync.WaitGroup
	configPath := flag.String("config", "config.json", "Path to configuration file")
	flag.Parse()

	config = loadConfig(*configPath)
	
	// Validate configuration
	if len(config.SourcePlexServers) == 0 {
		log.Fatalf("Error: No source Plex servers configured in config.json")
	}
	
	if config.AggregatorListenAddress == "" {
		log.Fatalf("Error: aggregator_listen_address not configured in config.json")
	}
	
	log.Printf("[main] Config loaded. AggregatorListenAddress: '%s'", config.AggregatorListenAddress)
	log.Printf("[main] Source Plex servers: %d configured", len(config.SourcePlexServers))
	for i, server := range config.SourcePlexServers {
		serverName := server.Name
		if serverName == "" {
			serverName = fmt.Sprintf("Server %d", i+1)
		}
		log.Printf("[main]   - %s: %s", serverName, server.URL)
	}

	// Create a root context that can be cancelled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start WebSocket server
	wg.Add(1)
	wsServer = startWebSocketServer(ctx, &wg)

	router := mux.NewRouter()
	apiRouter := router.PathPrefix("/").Subrouter()
	apiRouter.Use(authMiddleware) // Apply auth middleware

	apiRouter.HandleFunc("/", handlePlexRoot)
	apiRouter.HandleFunc("/status/sessions", handlePlexStatusSessions)
	apiRouter.HandleFunc("/ws", handleWebSocket)                         // For clients like Tautulli to connect for aggregated notifications (custom path)
	apiRouter.HandleFunc("/:/websockets/notifications", handleWebSocket) // Standard Plex path, handled by our aggregator
	apiRouter.HandleFunc("/status/sessions/history/all", handlePlexHistoryAll)
	apiRouter.HandleFunc("/identity", handleIdentity)
	apiRouter.HandleFunc("/{path:.*}", handlePlexAPIProxy)

	// Start WebSocket watchers
	for _, instance := range config.SourcePlexServers {
		wg.Add(1)
		go watchPlexInstance(ctx, &wg, instance, wsServer)
	}

	// Start history sync if configured
	if config.SyncIntervalMinutes > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			syncHistory()
		}()
	}

	// Setup graceful shutdown
	server := &http.Server{
		Addr:         config.ListenAddress,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[main] Server started and listening on %s", config.ListenAddress)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	log.Println("[main] Shutdown signal received, gracefully shutting down...")

	// Create shutdown context with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Shutdown HTTP server
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] Error shutting down server: %v", err)
	}

	// Cancel main context to stop all goroutines
	cancel()

	// Wait for all goroutines to finish
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("[main] All goroutines stopped, shutdown complete")
	case <-shutdownCtx.Done():
		log.Println("[main] Shutdown timeout exceeded")
	}
}

func handlePlexRoot(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r, false) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	log.Printf("Received request to root endpoint from %s", r.RemoteAddr)

	w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	machineID := generateMachineIdentifier(*config)

	// Return a proper Plex server identity response in XML format
	xmlResponse := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="1" allowCameraUpload="0" allowChannelAccess="1" allowMediaDeletion="1" allowSharing="1" allowSync="0" backgroundProcessing="1" certificate="1" companionProxy="1" countryCode="us" diagnostics="logs,databases,streaminglogs" eventStream="1" friendlyName="Plex Aggregator" hubSearch="1" itemClusters="1" livetv="7" machineIdentifier="%s" mediaProviders="1" multiuser="1" myPlex="1" myPlexMappingState="mapped" myPlexSigninState="ok" myPlexSubscription="1" myPlexUsername="aggregator" ownerFeatures="federated-auth,hardware-transcoding,home,hwtranscode,item_clusters,kevin-bacon,livetv,loudness,lyrics,music-analysis,music-fingerprinting,music-sonic-analysis,pass,photo-auto-tag,photos-v5,premium_music_metadata,radio,server-manager,session-bandwidth-restrictions,session-kick,shared-radio,sync,trailers,tuner-sharing,type-first,unsupportedtuners,webhooks" photoAutoTag="1" platform="Linux" platformVersion="5.15.0" pluginHost="1" pushNotifications="1" readOnlyLibraries="0" streamingBrainABRVersion="3" streamingBrainVersion="2" sync="1" transcoderActiveVideoSessions="0" transcoderAudio="1" transcoderLyrics="1" transcoderPhoto="1" transcoderSubtitles="1" transcoderVideo="1" transcoderVideoBitrates="64,96,208,320,720,1500,2000,3000,4000,8000,10000,12000,20000" transcoderVideoQualities="0,1,2,3,4,5,6,7,8,9,10,11,12" updatedAt="1707459729" updater="1" version="1.32.8.7639-aggregator" voiceSearch="1">
</MediaContainer>`, machineID)

	_, err := w.Write([]byte(xmlResponse))
	if err != nil {
		log.Printf("Error writing XML response: %v", err)
	}
}

func handlePlexStatusSessions(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received request to Plex API (sessions): %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
	if !checkAuth(r, false) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	// Aggregate active sessions from source Plex servers concurrently
	type sessionResult struct {
		sessions []PlexSessionItem
		server   string
		err      error
	}
	
	resultChan := make(chan sessionResult, len(config.SourcePlexServers))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	// Launch concurrent goroutines to fetch sessions
	for _, instance := range config.SourcePlexServers {
		go func(inst PlexInstanceConfig) {
			result := sessionResult{server: inst.Name}
			
			sessionsURL := fmt.Sprintf("%s/status/sessions", inst.URL)
			req, err := http.NewRequestWithContext(ctx, "GET", sessionsURL, nil)
			if err != nil {
				result.err = fmt.Errorf("error creating request: %w", err)
				resultChan <- result
				return
			}
			
			req.Header.Set("X-Plex-Token", inst.PlexToken)
			req.Header.Set("Accept", "application/xml")
			
			resp, err := insecureHTTPClient.Do(req)
			if err != nil {
				result.err = fmt.Errorf("error fetching sessions: %w", err)
				resultChan <- result
				return
			}
			defer resp.Body.Close()
			
			if resp.StatusCode != http.StatusOK {
				result.err = fmt.Errorf("non-OK status: %d", resp.StatusCode)
				resultChan <- result
				return
			}
			
			// Use buffer pool for better memory management
			buf := bufferPool.Get().(*bytes.Buffer)
			buf.Reset()
			defer bufferPool.Put(buf)
			
			_, err = io.Copy(buf, resp.Body)
			if err != nil {
				result.err = fmt.Errorf("error reading response: %w", err)
				resultChan <- result
				return
			}
			
			var sessionsContainer PlexSessionsMediaContainer
			if err := xml.Unmarshal(buf.Bytes(), &sessionsContainer); err != nil {
				result.err = fmt.Errorf("error unmarshalling XML: %w", err)
				resultChan <- result
				return
			}
			
			result.sessions = sessionsContainer.Items
			resultChan <- result
		}(instance)
	}
	
	// Collect results
	var allSessions []PlexSessionItem
	for i := 0; i < len(config.SourcePlexServers); i++ {
		result := <-resultChan
		if result.err != nil {
			log.Printf("Error fetching sessions from %s: %v", result.server, result.err)
			continue
		}
		log.Printf("Fetched %d active sessions from %s", len(result.sessions), result.server)
		allSessions = append(allSessions, result.sessions...)
	}
	
	// Create the aggregated response
	responseContainer := PlexSessionsMediaContainer{
		Size:  len(allSessions),
		Items: allSessions,
	}
	
	// Marshal to XML
	xmlBytes, err := xml.MarshalIndent(responseContainer, "", "  ")
	if err != nil {
		log.Printf("Error marshalling aggregated sessions to XML: %v", err)
		http.Error(w, "Failed to generate sessions response", http.StatusInternalServerError)
		return
	}
	
	// Add XML declaration
	xmlResponse := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + string(xmlBytes)
	
	_, err = w.Write([]byte(xmlResponse))
	if err != nil {
		log.Printf("Error writing sessions XML response: %v", err)
	}
	
	log.Printf("Returned %d aggregated sessions to client", len(allSessions))
}

func handlePlexHistoryAll(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received request to history endpoint: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
	if !checkAuth(r, false) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	
	// Read from cached merged history
	mergedHistoryMutex.RLock()
	historySnapshot := make([]PlexHistoryItem, len(mergedHistory))
	copy(historySnapshot, mergedHistory)
	mergedHistoryMutex.RUnlock()
	
	// Build response container
	responseContainer := PlexHistoryMediaContainer{
		Size:   len(historySnapshot),
		Videos: historySnapshot,
	}
	
	// Check Accept header to determine response format
	acceptHeader := r.Header.Get("Accept")
	
	if strings.Contains(acceptHeader, "application/json") {
		// Return JSON response
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		
		// Convert to JSON format
		jsonContainer := JSONPlexMediaContainer{
			MediaContainer: struct {
				Size     int                   `json:"size"`
				Metadata []JSONPlexHistoryItem `json:"Metadata"`
			}{
				Size:     len(historySnapshot),
				Metadata: make([]JSONPlexHistoryItem, 0, len(historySnapshot)),
			},
		}
		
		for _, item := range historySnapshot {
			jsonItem := JSONPlexHistoryItem{
				Key:                   item.Key,
				RatingKey:             item.RatingKey,
				ParentRatingKey:       item.ParentRatingKey,
				GrandparentRatingKey:  item.GrandparentRatingKey,
				Title:                 item.Title,
				GrandparentTitle:      item.GrandparentTitle,
				ParentTitle:           item.ParentTitle,
				Type:                  item.Type,
				Thumb:                 item.Thumb,
				Art:                   item.Art,
				ParentThumb:           item.ParentThumb,
				GrandparentThumb:      item.GrandparentThumb,
				OriginallyAvailableAt: item.OriginallyAvailableAt,
				ViewedAt:              item.ViewedAt,
				AccountID:             item.AccountID,
				DeviceID:              item.DeviceID,
			}
			jsonContainer.MediaContainer.Metadata = append(jsonContainer.MediaContainer.Metadata, jsonItem)
		}
		
		if err := json.NewEncoder(w).Encode(jsonContainer); err != nil {
			log.Printf("Error encoding history to JSON: %v", err)
			http.Error(w, "Failed to generate history response", http.StatusInternalServerError)
		}
	} else {
		// Default to XML response
		w.Header().Set("Content-Type", "text/xml;charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		
		xmlBytes, err := xml.MarshalIndent(responseContainer, "", "  ")
		if err != nil {
			log.Printf("Error marshalling history to XML: %v", err)
			http.Error(w, "Failed to generate history response", http.StatusInternalServerError)
			return
		}
		
		xmlResponse := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + string(xmlBytes)
		if _, err := w.Write([]byte(xmlResponse)); err != nil {
			log.Printf("Error writing history XML response: %v", err)
		}
	}
	
	log.Printf("Returned %d history items to client", len(historySnapshot))
}

func handlePlexAPIProxy(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r, false) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vars := mux.Vars(r)
	path := vars["path"]
	queryString := r.URL.RawQuery
	fullPath := path
	if queryString != "" {
		fullPath = path + "?" + queryString
	}
	log.Printf("Proxying request for: /%s (Method: %s) from %s", fullPath, r.Method, r.RemoteAddr)

	// Validate that we have at least one source server
	if len(config.SourcePlexServers) == 0 {
		log.Printf("Error: No source Plex servers configured")
		http.Error(w, "No source servers configured", http.StatusInternalServerError)
		return
	}

	// Try each source server until we get a successful response
	var lastError error
	var lastStatusCode int
	
	for _, server := range config.SourcePlexServers {
		proxyURL := fmt.Sprintf("%s/%s", server.URL, fullPath)
		log.Printf("Trying upstream server %s: %s", server.Name, proxyURL)

		req, err := http.NewRequest(r.Method, proxyURL, r.Body)
		if err != nil {
			log.Printf("Error creating proxy request for %s: %v", server.Name, err)
			lastError = err
			continue
		}

		// Copy headers from original request
		for k, v := range r.Header {
			req.Header[k] = v
		}
		req.Header.Set("X-Plex-Token", server.PlexToken)
		req.Header.Set("Accept-Encoding", "identity") // Explicitly request no compression from server

		resp, err := proxyHTTPClient.Do(req)
		if err != nil {
			log.Printf("Error proxying request to %s: %v", server.Name, err)
			lastError = err
			continue
		}
		defer resp.Body.Close()
		
		// If we get a 404, try the next server
		if resp.StatusCode == http.StatusNotFound {
			log.Printf("Got 404 from %s for /%s, trying next server", server.Name, fullPath)
			lastStatusCode = http.StatusNotFound
			continue
		}

		// For any other response (success or error), use this response
		log.Printf("Upstream response from %s for /%s: Status: %d", server.Name, fullPath, resp.StatusCode)

		// Use buffer pool for response body
		buf := bufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufferPool.Put(buf)
		
		_, err = io.Copy(buf, resp.Body)
		if err != nil {
			log.Printf("Error reading upstream response: %v", err)
			http.Error(w, "Error reading upstream response", http.StatusInternalServerError)
			return
		}

		bodyBytes := buf.Bytes()
		
		// Handle gzip decompression if needed
		upstreamContentEncoding := resp.Header.Get("Content-Encoding")
		if upstreamContentEncoding == "gzip" && len(bodyBytes) > 0 {
			gr, err := gzip.NewReader(bytes.NewReader(bodyBytes))
			if err != nil {
				log.Printf("Error creating gzip reader: %v", err)
			} else {
				decompressed := bufferPool.Get().(*bytes.Buffer)
				decompressed.Reset()
				defer bufferPool.Put(decompressed)
				
				_, err = io.Copy(decompressed, gr)
				gr.Close()
				if err == nil {
					bodyBytes = decompressed.Bytes()
				}
			}
		}

		// Copy response headers (except Content-Encoding and Content-Length)
		for k, v := range resp.Header {
			if k != "Content-Encoding" && k != "Content-Length" {
				w.Header()[k] = v
			}
		}

		// Set Content-Length based on actual body size
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))
		
		// Write status code
		w.WriteHeader(resp.StatusCode)

		// Write response body
		_, err = w.Write(bodyBytes)
		if err != nil {
			log.Printf("Error writing response: %v", err)
		}
		
		log.Printf("Successfully proxied %d bytes from %s for /%s", len(bodyBytes), server.Name, fullPath)
		return
	}

	// If all servers failed
	if lastStatusCode == http.StatusNotFound {
		http.Error(w, "Not found on any server", http.StatusNotFound)
	} else if lastError != nil {
		log.Printf("All servers failed, last error: %v", lastError)
		http.Error(w, "Failed to proxy request", http.StatusBadGateway)
	} else {
		http.Error(w, "Unknown error", http.StatusInternalServerError)
	}
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	log.Printf("WebSocket connection attempt from %s for %s with query %s", r.RemoteAddr, r.URL.Path, r.URL.RawQuery)

	if !checkAuth(r, true) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
	if err != nil {
		log.Printf("Failed to upgrade connection to WebSocket: %v", err)
		http.Error(w, "Failed to upgrade connection", http.StatusInternalServerError)
		return
	}

	client := &WebSocketClient{
		conn:   conn,
		send:   make(chan []byte, 256),
		remote: r.RemoteAddr,
	}

	wsServer.register <- client
	defer func() {
		wsServer.unregister <- client
		close(client.send)
		conn.Close()
	}()

	identity := WebSocketIdentityNotification{
		NotificationContainer: WebSocketIdentityContainer{
			Size:   1,
			Type:   "identity",
			Server: WebSocketIdentityServerInfo{
				FriendlyName:      "Plex Aggregator",
				MachineIdentifier: generateMachineIdentifier(*config),
			},
		},
	}
	identityBytes, _ := json.Marshal(identity)
	wsServer.broadcast <- identityBytes

	go func() {
		for message := range client.send {
			err := conn.WriteMessage(websocket.TextMessage, message)
			if err != nil {
				log.Printf("Error writing message to WebSocket client %s: %v", client.remote, err)
				return
			}
		}
	}()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WebSocket read error: %v", err)
			break
		}
	}

	disconnectMsg := map[string]interface{}{
		"NotificationContainer": map[string]interface{}{
			"size": 0,
		},
	}
	disconnectBytes, _ := json.Marshal(disconnectMsg)
	wsServer.broadcast <- disconnectBytes
}

func handleIdentity(w http.ResponseWriter, r *http.Request) {
	identity := struct {
		MachineIdentifier string `json:"machineIdentifier"`
		Version           string `json:"version"`
		Platform          string `json:"platform"`
		PlatformVersion   string `json:"platformVersion"`
		Device            string `json:"device"`
		Model             string `json:"model"`
		Product           string `json:"product"`
		Vendor            string `json:"vendor"`
	}{
		MachineIdentifier: config.MachineIdentifier,
		Version:           "1.0.0",
		Platform:          "MulTau",
		PlatformVersion:   "1.0.0",
		Device:            "MulTau Aggregator",
		Model:             "Aggregator",
		Product:           "MulTau",
		Vendor:            "MulTau",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(identity)
}

func watchPlexInstance(ctx context.Context, wg *sync.WaitGroup, instanceConfig PlexInstanceConfig, wsServer *WebSocketServer) {
	defer wg.Done()

	// Construct WebSocket URL with proper scheme
	baseURL := instanceConfig.URL
	wsSchemeURL := baseURL
	if strings.HasPrefix(baseURL, "http://") {
		wsSchemeURL = "ws://" + strings.TrimPrefix(baseURL, "http://")
	} else if strings.HasPrefix(baseURL, "https://") {
		wsSchemeURL = "wss://" + strings.TrimPrefix(baseURL, "https://")
	} else if !strings.HasPrefix(baseURL, "ws://") && !strings.HasPrefix(baseURL, "wss://") {
		log.Printf("Warning: Plex instance URL '%s' for %s has no scheme, using ws://", baseURL, instanceConfig.Name)
		wsSchemeURL = "ws://" + baseURL
	}

	wsURL := fmt.Sprintf("%s/:/websockets/notifications?X-Plex-Token=%s", wsSchemeURL, instanceConfig.PlexToken)
	u, err := url.Parse(wsURL)
	if err != nil {
		log.Printf("Error parsing WebSocket URL for %s: %v", instanceConfig.Name, err)
		return
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	if u.Scheme == "wss" {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	// Exponential backoff parameters
	minBackoff := 1 * time.Second
	maxBackoff := 5 * time.Minute
	backoff := minBackoff
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			log.Printf("Stopping WebSocket watcher for %s", instanceConfig.Name)
			return
		default:
			log.Printf("Attempting WebSocket connection to %s", instanceConfig.Name)
			
			conn, resp, err := dialer.DialContext(ctx, u.String(), nil)
			if err != nil {
				consecutiveFailures++
				log.Printf("Failed to connect to %s WebSocket (attempt %d): %v", instanceConfig.Name, consecutiveFailures, err)
				
				// Exponential backoff
				time.Sleep(backoff)
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			
			// Connection successful, reset backoff
			backoff = minBackoff
			consecutiveFailures = 0
			
			if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
				log.Printf("Unexpected HTTP response from %s: %s", instanceConfig.Name, resp.Status)
				resp.Body.Close()
			}
			
			log.Printf("Successfully connected to %s WebSocket", instanceConfig.Name)
			
			// Handle WebSocket messages
			conn.SetReadDeadline(time.Time{}) // No read deadline
			conn.SetPongHandler(func(string) error {
				conn.SetReadDeadline(time.Now().Add(60 * time.Second))
				return nil
			})
			
			// Start ping ticker
			ticker := time.NewTicker(30 * time.Second)
			go func() {
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
							log.Printf("Error sending ping to %s: %v", instanceConfig.Name, err)
							conn.Close()
							return
						}
					case <-ctx.Done():
						return
					}
				}
			}()
			
			// Read messages
			for {
				_, message, err := conn.ReadMessage()
				if err != nil {
					log.Printf("Error reading message from %s: %v", instanceConfig.Name, err)
					conn.Close()
					ticker.Stop()
					break
				}

				// Parse and forward message
				event := PlexEvent{}
				if err := json.Unmarshal(message, &event); err == nil {
					event.SourceServer = instanceConfig.Name
					modifiedMessage, _ := json.Marshal(event)
					
					select {
					case wsServer.broadcast <- modifiedMessage:
					case <-ctx.Done():
						conn.Close()
						return
					default:
						log.Printf("Warning: broadcast channel full, dropping message from %s", instanceConfig.Name)
					}
				} else {
					log.Printf("Failed to parse WebSocket message from %s: %v", instanceConfig.Name, err)
				}
			}
			
			// Connection closed, wait before reconnecting
			time.Sleep(backoff)
		}
	}
}

func syncHistory() {
	for {
		log.Println("Starting history sync cycle...")
		
		// Use context for proper cancellation
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		
		type historyResult struct {
			items  []PlexHistoryItem
			server string
			err    error
		}
		
		resultChan := make(chan historyResult, len(config.SourcePlexServers))
		
		// Fetch history from all servers concurrently
		for _, instance := range config.SourcePlexServers {
			go func(inst PlexInstanceConfig) {
				result := historyResult{server: inst.Name}
				
				historyURL := fmt.Sprintf("%s/status/sessions/history/all", inst.URL)
				req, err := http.NewRequestWithContext(ctx, "GET", historyURL, nil)
				if err != nil {
					result.err = fmt.Errorf("error creating request: %w", err)
					resultChan <- result
					return
				}
				
				req.Header.Set("X-Plex-Token", inst.PlexToken)
				req.Header.Set("Accept", "application/json")
				
				resp, err := insecureHTTPClient.Do(req)
				if err != nil {
					result.err = fmt.Errorf("error fetching history: %w", err)
					resultChan <- result
					return
				}
				defer resp.Body.Close()
				
				if resp.StatusCode != http.StatusOK {
					result.err = fmt.Errorf("status code %d", resp.StatusCode)
					resultChan <- result
					return
				}
				
				// Use buffer pool
				buf := bufferPool.Get().(*bytes.Buffer)
				buf.Reset()
				defer bufferPool.Put(buf)
				
				_, err = io.Copy(buf, resp.Body)
				if err != nil {
					result.err = fmt.Errorf("error reading response: %w", err)
					resultChan <- result
					return
				}
				
				// Try XML first, then JSON
				var historyContainer PlexHistoryMediaContainer
				if err := xml.Unmarshal(buf.Bytes(), &historyContainer); err == nil {
					for i := range historyContainer.Videos {
						historyContainer.Videos[i].SourceServerName = inst.Name
					}
					result.items = historyContainer.Videos
					log.Printf("Fetched %d history items (XML) from %s", len(result.items), inst.Name)
				} else {
					// Try JSON
					var jsonHistoryContainer JSONPlexMediaContainer
					if err := json.Unmarshal(buf.Bytes(), &jsonHistoryContainer); err == nil {
						result.items = make([]PlexHistoryItem, 0, len(jsonHistoryContainer.MediaContainer.Metadata))
						for _, jsonItem := range jsonHistoryContainer.MediaContainer.Metadata {
							historyItem := PlexHistoryItem{
								Key:                   jsonItem.Key,
								RatingKey:             jsonItem.RatingKey,
								ParentRatingKey:       jsonItem.ParentRatingKey,
								GrandparentRatingKey:  jsonItem.GrandparentRatingKey,
								Title:                 jsonItem.Title,
								GrandparentTitle:      jsonItem.GrandparentTitle,
								ParentTitle:           jsonItem.ParentTitle,
								Type:                  jsonItem.Type,
								Thumb:                 jsonItem.Thumb,
								Art:                   jsonItem.Art,
								ParentThumb:           jsonItem.ParentThumb,
								GrandparentThumb:      jsonItem.GrandparentThumb,
								OriginallyAvailableAt: jsonItem.OriginallyAvailableAt,
								ViewedAt:              jsonItem.ViewedAt,
								AccountID:             jsonItem.AccountID,
								DeviceID:              jsonItem.DeviceID,
								SourceServerName:      inst.Name,
							}
							result.items = append(result.items, historyItem)
						}
						log.Printf("Fetched %d history items (JSON) from %s", len(result.items), inst.Name)
					} else {
						result.err = fmt.Errorf("failed to unmarshal as XML or JSON")
					}
				}
				
				resultChan <- result
			}(instance)
		}
		
		// Collect all results
		var allHistory []PlexHistoryItem
		for i := 0; i < len(config.SourcePlexServers); i++ {
			result := <-resultChan
			if result.err != nil {
				log.Printf("Error fetching history from %s: %v", result.server, result.err)
				continue
			}
			allHistory = append(allHistory, result.items...)
		}
		
		cancel() // Clean up context
		
		// Sort by ViewedAt descending (most recent first)
		sort.SliceStable(allHistory, func(i, j int) bool {
			return allHistory[i].ViewedAt > allHistory[j].ViewedAt
		})
		
		// Efficient deduplication using map with composite key
		seen := make(map[string]struct{})
		uniqueHistory := make([]PlexHistoryItem, 0, len(allHistory))
		
		for _, item := range allHistory {
			// Create a more comprehensive deduplication key
			key := fmt.Sprintf("%s|%d|%d|%s", item.RatingKey, item.ViewedAt, item.AccountID, item.SourceServerName)
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				uniqueHistory = append(uniqueHistory, item)
			}
		}
		
		// Update global history with lock
		mergedHistoryMutex.Lock()
		mergedHistory = uniqueHistory
		mergedHistoryMutex.Unlock()
		
		log.Printf("History sync complete. Total items: %d, Unique items: %d", len(allHistory), len(uniqueHistory))
		
		// Check if we should continue syncing
		if config.SyncIntervalMinutes <= 0 {
			log.Println("SyncIntervalMinutes not configured, history sync will run once")
			return
		}
		
		// Wait for next sync cycle
		time.Sleep(time.Duration(config.SyncIntervalMinutes) * time.Minute)
	}
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(r, false) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
