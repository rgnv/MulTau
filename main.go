package main

import (
	"encoding/xml"
	"sort"
	"strings"
	"bytes"
	"context"
	"crypto/tls"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

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
	SyncIntervalMinutes     int                  `json:"sync_interval_minutes"` // Retained for potential periodic history fetching from source Plex servers
	LogLevel                string               `json:"log_level,omitempty"`
	AggregatorPlexToken     string               `json:"aggregator_plex_token,omitempty"` // Token clients must use to connect to this aggregator
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

type WebSocketServer struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan []byte
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	mu         sync.Mutex
}

func NewWebSocketServer() *WebSocketServer {
	return &WebSocketServer{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}
}

func (s *WebSocketServer) run() {
	for {
		select {
		case client := <-s.register:
			s.mu.Lock()
			s.clients[client] = true
			s.mu.Unlock()
		case client := <-s.unregister:
			s.mu.Lock()
			if _, ok := s.clients[client]; ok {
				delete(s.clients, client)
				client.Close()
			}
			s.mu.Unlock()
		case message := <-s.broadcast:
			s.mu.Lock()
			for client := range s.clients {
				if err := client.WriteMessage(websocket.TextMessage, message); err != nil {
					log.Printf("error: %v", err)
					client.Close()
					delete(s.clients, client)
				}
			}
			s.mu.Unlock()
		}
	}
}

func loadConfig() (*Config, error) {
	configFile, err := os.Open("config.json")
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	defer configFile.Close()

	var config Config
	err = json.NewDecoder(configFile).Decode(&config)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	log.Printf("[loadConfig] Loaded AggregatorListenAddress: '%s'", config.AggregatorListenAddress) // Debug log
	return &config, nil
}

func main() {
	var err error
	config, err = loadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	
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

	wsServer = NewWebSocketServer()

	router := mux.NewRouter()
	apiRouter := router.PathPrefix("/").Subrouter()

	apiRouter.HandleFunc("/", handlePlexRoot)
	apiRouter.HandleFunc("/status/sessions", handlePlexStatusSessions)
	apiRouter.HandleFunc("/ws", handleWebSocket)                         // For clients like Tautulli to connect for aggregated notifications (custom path)
	apiRouter.HandleFunc("/:/websockets/notifications", handleWebSocket) // Standard Plex path, handled by our aggregator
	apiRouter.HandleFunc("/status/sessions/history/all", handlePlexHistoryAll)
	apiRouter.HandleFunc("/{path:.*}", handlePlexAPIProxy)

	go wsServer.run()

	var wg sync.WaitGroup
	for _, instance := range config.SourcePlexServers {
		wg.Add(1)
		go watchPlexInstance(context.Background(), &wg, instance, wsServer)
	}

	go syncHistory()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdown
		log.Println("Shutting down...")
		wg.Wait()
		os.Exit(0)
	}()

	log.Printf("Starting Plex Aggregator on %s", config.AggregatorListenAddress)
	if err := http.ListenAndServe(config.AggregatorListenAddress, router); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func generateMachineIdentifier() string {
	return "PlexAggregator"
}

func handlePlexRoot(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r, false) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	log.Printf("Received request to root endpoint from %s", r.RemoteAddr)

	w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	machineID := generateMachineIdentifier()

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
	
	// Aggregate active sessions from source Plex servers
	var allSessions []PlexSessionItem
	
	httpTransport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: httpTransport,
	}
	
	for _, instance := range config.SourcePlexServers {
		sessionsURL := fmt.Sprintf("%s/status/sessions", instance.URL)
		req, err := http.NewRequest("GET", sessionsURL, nil)
		if err != nil {
			log.Printf("Error creating request for sessions from %s: %v", instance.Name, err)
			continue
		}
		
		req.Header.Set("X-Plex-Token", instance.PlexToken)
		req.Header.Set("Accept", "application/xml")
		
		resp, err := httpClient.Do(req)
		if err != nil {
			log.Printf("Error fetching sessions from %s: %v", instance.Name, err)
			continue
		}
		defer resp.Body.Close()
		
		if resp.StatusCode != http.StatusOK {
			log.Printf("Non-OK status from %s sessions endpoint: %d", instance.Name, resp.StatusCode)
			continue
		}
		
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Error reading sessions response from %s: %v", instance.Name, err)
			continue
		}
		
		var sessionsContainer PlexSessionsMediaContainer
		if err := xml.Unmarshal(body, &sessionsContainer); err != nil {
			log.Printf("Error unmarshalling sessions XML from %s: %v", instance.Name, err)
			// Log a sample of the body for debugging
			var bodySample string
			if len(body) > 500 {
				bodySample = string(body[:500])
			} else {
				bodySample = string(body)
			}
			log.Printf("Sessions response body sample from %s: %s", instance.Name, bodySample)
			continue
		}
		
		log.Printf("Fetched %d active sessions from %s", sessionsContainer.Size, instance.Name)
		allSessions = append(allSessions, sessionsContainer.Items...)
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
	log.Printf("Received request for /status/sessions/history/all from %s", r.RemoteAddr)
	if !checkAuth(r, false) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	mergedHistoryMutex.RLock()
	defer mergedHistoryMutex.RUnlock()

	// Create a container to hold the history items for XML marshalling
	// Tautulli expects certain attributes in the MediaContainer for history.
	historyContainer := PlexHistoryMediaContainer{
		Size:             len(mergedHistory),
		AllowSync:        true, // Typical value seen in Plex responses
		Identifier:       "com.plexapp.plugins.library", // Standard identifier
		MediaTagPrefix:   "/system/bundle/media/flags/", // Common prefix
		MediaTagVersion:  time.Now().Unix(), // Needs to be a timestamp like value
		Videos:           make([]PlexHistoryItem, len(mergedHistory)),
	}
	copy(historyContainer.Videos, mergedHistory) // Copy to avoid issues if mergedHistory is modified elsewhere

	w.Header().Set("Content-Type", "application/xml;charset=utf-8")
	xmlBytes, err := xml.MarshalIndent(historyContainer, "", "  ")
	if err != nil {
		log.Printf("Error marshalling history to XML: %v", err)
		http.Error(w, "Failed to generate history response", http.StatusInternalServerError)
		return
	}

	_, err = w.Write(xmlBytes)
	if err != nil {
		log.Printf("Error writing history XML response: %v", err)
	}
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

	transport := &http.Transport{
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true},
		DisableCompression: true,
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
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

		for k, v := range r.Header {
			req.Header[k] = v
		}
		req.Header.Set("X-Plex-Token", server.PlexToken)
		req.Header.Set("Accept-Encoding", "identity") // Explicitly request no compression from server

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Error proxying request to %s: %v", server.Name, err)
			lastError = err
			continue
		}
		
		// If we get a 404, try the next server
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			log.Printf("Got 404 from %s for /%s, trying next server", server.Name, fullPath)
			lastStatusCode = http.StatusNotFound
			continue
		}

		// For any other response (success or error), use this response
		defer resp.Body.Close()
		
		log.Printf("Upstream response from %s for /%s: Status: [%s], StatusCode: [%d], Proto: [%s], ContentLength: [%d], Uncompressed: [%t]", 
			server.Name, fullPath, resp.Status, resp.StatusCode, resp.Proto, resp.ContentLength, resp.Uncompressed)
		log.Printf("Upstream response headers for /%s:", fullPath)
		for name, values := range resp.Header {
			for _, value := range values {
				log.Printf("  HEADER %s: %s", name, value)
			}
		}
		log.Printf("Finished logging upstream headers for /%s. Now reading body.", fullPath)

		// Read the full body once - REMOVED the flawed TeeReader approach
		processedBodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Error reading full upstream response body: %v", err)
			http.Error(w, "Error reading upstream response", http.StatusInternalServerError)
			return
		}

		upstreamContentEncoding := resp.Header.Get("Content-Encoding")
		log.Printf("Read %d bytes from upstream for /%s (Content-Type: %s, Original Content-Encoding: %s)", len(processedBodyBytes), fullPath, resp.Header.Get("Content-Type"), upstreamContentEncoding)

		// Manually decompress if upstream sent gzip (DisableCompression on transport means client didn't ask for/handle it)
		if upstreamContentEncoding == "gzip" && len(processedBodyBytes) > 0 {
			gr, gzipErr := gzip.NewReader(bytes.NewReader(processedBodyBytes))
			if gzipErr != nil {
				log.Printf("Error creating gzip reader for /%s: %v. Sending empty body to client.", fullPath, gzipErr)
				processedBodyBytes = []byte{} // Clear body on error
			} else {
				decompressedBytes, readErr := io.ReadAll(gr)
				gr.Close() // Close reader regardless of readErr
				if readErr != nil {
					log.Printf("Error decompressing gzipped body for /%s: %v. Sending empty body to client.", fullPath, readErr)
					processedBodyBytes = []byte{} // Clear body on error
				} else {
					log.Printf("Manually decompressed gzipped body from upstream for /%s. Original size: %d, Decompressed size: %d", fullPath, len(processedBodyBytes), len(decompressedBytes))
					processedBodyBytes = decompressedBytes // Replace with decompressed data
				}
			}
		}

		// Log a snippet of the (potentially decompressed) body
		logSnippet := string(processedBodyBytes)
		if len(logSnippet) > 512 { // Truncate for logging
			logSnippet = logSnippet[:512] + "... (truncated)"
		}
		log.Printf("Proxying (potentially decompressed) upstream response body snippet for /%s:\n%s", fullPath, logSnippet)

		// Set headers and status code
		// We will not forward Content-Encoding or Content-Length from upstream,
		// as we are sending plain content and net/http will set the correct length.
		for k, v := range resp.Header {
			if strings.EqualFold(k, "Content-Encoding") || strings.EqualFold(k, "Content-Length") {
				continue // Skip these headers, we send uncompressed and net/http sets length
			}
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)

		// Special handling for /library/sections if upstream gives empty body for XML (after potential decompression)
		if (path == "library/sections" || strings.HasPrefix(path, "library/sections?")) &&
			resp.StatusCode == http.StatusOK &&
			len(processedBodyBytes) == 0 && // Check length of processed (potentially decompressed) body
			strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "xml") {

			log.Printf("Warning: Upstream server %s returned 200 OK with an empty XML body for /%s. Sending default empty MediaContainer.", server.Name, fullPath)
			// Send a minimal valid XML response
			defaultXML := `<?xml version="1.0" encoding="UTF-8"?><MediaContainer size="0"></MediaContainer>`
			processedBodyBytes = []byte(defaultXML)
			// Update Content-Type to ensure it's set correctly
			w.Header().Set("Content-Type", "text/xml;charset=utf-8")
		}

		// Write the (potentially decompressed) body
		if len(processedBodyBytes) > 0 {
			written, writeErr := w.Write(processedBodyBytes)
			if writeErr != nil {
				log.Printf("Error writing response body for /%s: %v", fullPath, writeErr)
			} else {
				log.Printf("Successfully wrote %d bytes to client for /%s", written, fullPath)
			}
		} else {
			log.Printf("No body to write for /%s (0 bytes after processing)", fullPath)
		}
		
		// Successfully handled the request with this server
		return
	}
	
	// If we get here, all servers failed or returned 404
	if lastStatusCode == http.StatusNotFound {
		log.Printf("All servers returned 404 for /%s", fullPath)
		http.Error(w, "Not Found", http.StatusNotFound)
	} else if lastError != nil {
		log.Printf("All servers failed for /%s. Last error: %v", fullPath, lastError)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	} else {
		log.Printf("Unexpected state: no servers processed request for /%s", fullPath)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
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

	wsServer.register <- conn
	defer func() {
		wsServer.unregister <- conn
		conn.Close()
	}()

	identity := WebSocketIdentityNotification{
		NotificationContainer: WebSocketIdentityContainer{
			Size:   1,
			Type:   "identity",
			Server: WebSocketIdentityServerInfo{
				FriendlyName:      "Plex Aggregator",
				MachineIdentifier: generateMachineIdentifier(),
			},
		},
	}
	identityBytes, _ := json.Marshal(identity)
	wsServer.broadcast <- identityBytes

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

func watchPlexInstance(ctx context.Context, wg *sync.WaitGroup, instanceConfig PlexInstanceConfig, wsServer *WebSocketServer) {
	defer wg.Done()

	// Construct WebSocket URL, ensuring correct scheme (ws/wss)
	baseURL := instanceConfig.URL
	wsSchemeURL := baseURL
	if strings.HasPrefix(baseURL, "http://") {
		wsSchemeURL = "ws://" + strings.TrimPrefix(baseURL, "http://")
	} else if strings.HasPrefix(baseURL, "https://") {
		wsSchemeURL = "wss://" + strings.TrimPrefix(baseURL, "https://")
	} else if !strings.HasPrefix(baseURL, "ws://") && !strings.HasPrefix(baseURL, "wss://") {
		// If no scheme, assume ws for now or consider it an error / require explicit ws/wss in config
		// For now, let's log a warning if no http/https/ws/wss prefix found and try to prepend ws://
		log.Printf("Warning: Plex instance URL '%s' for %s has no scheme, attempting ws://", baseURL, instanceConfig.Name)
		wsSchemeURL = "ws://" + baseURL
	}

	// Path: /:/websockets/notifications. Token in query ONLY. No custom dial headers (mimicking python-plexapi).
	wsURL := fmt.Sprintf("%s/:/websockets/notifications?X-Plex-Token=%s", wsSchemeURL, instanceConfig.PlexToken)
	u, err := url.Parse(wsURL)
	if err != nil {
		log.Printf("Error parsing WebSocket URL %s for %s: %v", wsURL, instanceConfig.Name, err)
		return
	}
	log.Printf("Prepared WebSocket URL for %s: %s", instanceConfig.Name, u.String())

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second, // Explicit handshake timeout
	}
	if u.Scheme == "wss" {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			log.Printf("Attempting WebSocket connection to %s at %s", instanceConfig.Name, u.String())
			log.Printf("[%s] Dialing WebSocket...", instanceConfig.Name)
			conn, resp, err := dialer.Dial(u.String(), nil)
			log.Printf("[%s] Dial completed. Error: %v", instanceConfig.Name, err)
			if err != nil {
				log.Printf("Failed to connect to %s WebSocket: %v. Retrying in %v...", instanceConfig.Name, err, 5*time.Second)
				time.Sleep(5 * time.Second) // Exponential backoff
				continue
			}
			if resp != nil {
				log.Printf("[%s] HTTP response status: %s", instanceConfig.Name, resp.Status)
				b, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096)) // Limit read to avoid large output
				if readErr != nil {
					log.Printf("[%s] Error reading response body: %v", instanceConfig.Name, readErr)
				} else {
					log.Printf("[%s] HTTP response body: %s", instanceConfig.Name, string(b))
				}
				resp.Body.Close()
			}

			for {
				_, message, err := conn.ReadMessage()
				if err != nil {
					log.Printf("Error reading message from %s: %v", instanceConfig.Name, err)
					conn.Close()
					break
				}

				event := PlexEvent{}
				if err := json.Unmarshal(message, &event); err == nil {
					event.SourceServer = instanceConfig.Name
					modifiedMessage, _ := json.Marshal(event)
					wsServer.broadcast <- modifiedMessage
				} else {
					log.Printf("Failed to parse message from %s: %v", instanceConfig.Name, err)
				}
			}

			time.Sleep(5 * time.Second)
		}
	}
}

func syncHistory() {
	for {
		log.Println("Starting history sync cycle...")
		var currentCycleHistory []PlexHistoryItem

		httpTransport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // Skip TLS verification for IPs/self-signed certs
		}
		httpClient := &http.Client{
			Timeout:   10 * time.Second,
			Transport: httpTransport,
		}

		for _, instance := range config.SourcePlexServers {
			historyURL := fmt.Sprintf("%s/status/sessions/history/all", instance.URL)
			req, err := http.NewRequest("GET", historyURL, nil)
			if err != nil {
				log.Printf("Error creating request for %s history: %v", instance.Name, err)
				continue
			}
			req.Header.Set("X-Plex-Token", instance.PlexToken)
			req.Header.Set("Accept", "application/json") // Request JSON, easier to parse initially

			resp, err := httpClient.Do(req)
			if err != nil {
				log.Printf("Error fetching history from %s: %v", instance.Name, err)
				continue
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				log.Printf("Error fetching history from %s: status code %d", instance.Name, resp.StatusCode)
				continue
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Error reading history response body from %s: %v", instance.Name, err)
				continue
			}

			// Plex often returns XML, but we requested JSON. If it's XML, we'll need to adjust.
			// For now, let's assume we can get JSON or adapt if Plex forces XML.
			// The Plex API can be inconsistent with the 'Accept' header for this endpoint.
			// We will try to unmarshal as PlexHistoryMediaContainer (which expects XML tags)
			var historyContainer PlexHistoryMediaContainer // Single declaration
			if err := xml.Unmarshal(body, &historyContainer); err == nil {
				for _, item := range historyContainer.Videos {
					item.SourceServerName = instance.Name
					currentCycleHistory = append(currentCycleHistory, item)
				}
				log.Printf("Fetched %d history items (XML) from %s", len(historyContainer.Videos), instance.Name)
			} else {
				log.Printf("Failed to unmarshal history as XML from %s (will try JSON): %v", instance.Name, err)
				var jsonHistoryContainer JSONPlexMediaContainer
				if err := json.Unmarshal(body, &jsonHistoryContainer); err == nil {
					log.Printf("Successfully unmarshalled history as JSON from %s. Items: %d", instance.Name, jsonHistoryContainer.MediaContainer.Size)
					for _, jsonItem := range jsonHistoryContainer.MediaContainer.Metadata {
						// Convert JSONPlexHistoryItem to PlexHistoryItem
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
							SourceServerName:      instance.Name,
						}
						currentCycleHistory = append(currentCycleHistory, historyItem)
					}
					log.Printf("Fetched %d history items (JSON converted) from %s", len(jsonHistoryContainer.MediaContainer.Metadata), instance.Name)
				} else {
					// Log the body if both XML and JSON unmarshal fail for debugging
					var bodySample string
					if len(body) > 500 {
						bodySample = string(body[:500])
					} else {
						bodySample = string(body)
					}
					log.Printf("Error unmarshalling history from %s as XML and JSON. JSON error: %v. Body sample: %s", instance.Name, err, bodySample)
					continue // Skip this instance if parsing fails for both
				}
			}

			// This logging was inside the XML part, moved out or handled by new logic
			// if xmlParsed {
			// 	 log.Printf("Fetched %d history items from %s", len(historyContainer.Videos), instance.Name)
			// }
		}




		// Sort by ViewedAt descending (most recent first)
		sort.SliceStable(currentCycleHistory, func(i, j int) bool {
			return currentCycleHistory[i].ViewedAt > currentCycleHistory[j].ViewedAt
		})

		// Deduplicate (simple approach: keep first seen based on a unique key, e.g., RatingKey + ViewedAt + AccountID)
		// More sophisticated deduplication might be needed if items can truly be identical across servers but mean different views.
		seen := make(map[string]bool)
		var uniqueHistory []PlexHistoryItem
		for _, item := range currentCycleHistory {
			// Using RatingKey and ViewedAt as a composite key for deduplication for now.
			// AccountID might also be necessary if different users on different servers watch the same thing at the same time.
			duplicateKey := fmt.Sprintf("%s-%d-%d", item.RatingKey, item.ViewedAt, item.AccountID)
			if !seen[duplicateKey] {
				seen[duplicateKey] = true
				uniqueHistory = append(uniqueHistory, item)
			}
		}

		mergedHistoryMutex.Lock()
		mergedHistory = uniqueHistory
		mergedHistoryMutex.Unlock()

		log.Printf("History sync cycle complete. Total merged and unique items: %d", len(mergedHistory))

		if config.SyncIntervalMinutes <= 0 {
			log.Println("SyncIntervalMinutes is not configured or is invalid. History sync will run once then stop.")
			return // Exits the goroutine
		}
		time.Sleep(time.Duration(config.SyncIntervalMinutes) * time.Minute)
	}
}
