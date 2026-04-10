package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/nullable-eth/labelarr/internal/config"
	"github.com/nullable-eth/labelarr/internal/media"
	"github.com/nullable-eth/labelarr/internal/plex"
)

const (
	eventLibraryNew    = "library.new"
	eventLibraryOnDeck = "library.on.deck"
)

// PlexWebhookPayload matches the Plex webhook JSON structure.
// Plex sends this as the "payload" field in a multipart/form-data POST.
// Requires Plex Pass on the server.
type PlexWebhookPayload struct {
	Event   string `json:"event"`
	User    bool   `json:"user"`
	Owner   bool   `json:"owner"`
	Account struct {
		ID    int    `json:"id"`
		Title string `json:"title"`
	} `json:"Account"`
	Server struct {
		Title string `json:"title"`
		UUID  string `json:"uuid"`
	} `json:"Server"`
	Player struct {
		Local bool   `json:"local"`
		Title string `json:"title"`
		UUID  string `json:"uuid"`
	} `json:"Player"`
	Metadata struct {
		LibrarySectionType  string `json:"librarySectionType"`
		LibrarySectionID    int    `json:"librarySectionID"`
		LibrarySectionTitle string `json:"librarySectionTitle"`
		RatingKey           string `json:"ratingKey"`
		Key                 string `json:"key"`
		GUID                string `json:"guid"`
		Type                string `json:"type"`
		Title               string `json:"title"`
		Year                int    `json:"year"`
		AddedAt             int64  `json:"addedAt"`
	} `json:"Metadata"`
}

type libraryInfo struct {
	name      string
	mediaType media.MediaType
}

type Server struct {
	config         *config.Config
	processor      *media.Processor
	httpServer     *http.Server
	libraryMap     map[string]libraryInfo
	debounceTimers map[string]*time.Timer
	debounceGen    map[string]uint64
	debounceMu     sync.Mutex
}

func NewServer(cfg *config.Config, proc *media.Processor, movieLibs, tvLibs []plex.Library) *Server {
	libMap := make(map[string]libraryInfo, len(movieLibs)+len(tvLibs))
	for _, lib := range movieLibs {
		libMap[lib.Key] = libraryInfo{name: lib.Title, mediaType: media.MediaTypeMovie}
	}
	for _, lib := range tvLibs {
		libMap[lib.Key] = libraryInfo{name: lib.Title, mediaType: media.MediaTypeTV}
	}

	return &Server{
		config:         cfg,
		processor:      proc,
		libraryMap:     libMap,
		debounceTimers: make(map[string]*time.Timer),
		debounceGen:    make(map[string]uint64),
	}
}

// Start binds the webhook port and begins serving. Returns an error if the port
// cannot be bound, so the caller knows immediately if startup failed.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.handleWebhook)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	addr := fmt.Sprintf(":%d", s.config.WebhookPort)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to bind webhook port %d: %w", s.config.WebhookPort, err)
	}

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Webhook server error: %v\n", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the webhook server.
func (s *Server) Stop(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Plex sends multipart/form-data with a "payload" JSON field
	// and optionally a thumbnail image
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	payloadStr := r.FormValue("payload")
	if payloadStr == "" {
		http.Error(w, "Missing payload", http.StatusBadRequest)
		return
	}

	var payload PlexWebhookPayload
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	if s.config.VerboseLogging {
		fmt.Printf("Webhook received: event=%s library=%s section_type=%s media_type=%s title=%s\n",
			payload.Event,
			payload.Metadata.LibrarySectionTitle,
			payload.Metadata.LibrarySectionType,
			payload.Metadata.Type,
			payload.Metadata.Title)
	}

	switch payload.Event {
	case eventLibraryNew, eventLibraryOnDeck:
		// These are the events fired when new media is added
	default:
		w.WriteHeader(http.StatusOK)
		return
	}

	libraryID := strconv.Itoa(payload.Metadata.LibrarySectionID)

	// Use LibrarySectionType to determine media type, falling back to library map.
	// Plex Metadata.Type is "movie", "episode", "track" etc. but
	// LibrarySectionType is "movie" or "show" which maps to our processing paths.
	mediaType := s.resolveMediaType(libraryID, payload.Metadata.LibrarySectionType)
	libraryName := payload.Metadata.LibrarySectionTitle
	if libraryName == "" {
		if info, ok := s.libraryMap[libraryID]; ok {
			libraryName = info.name
		}
	}

	if mediaType != media.MediaTypeUnknown {
		s.scheduleProcessing(libraryID, libraryName, mediaType)
	} else if s.config.VerboseLogging {
		fmt.Printf("Webhook: ignoring event for unknown library %s (ID: %s)\n", libraryName, libraryID)
	}

	w.WriteHeader(http.StatusOK)
}

// resolveMediaType determines the MediaType from the Plex LibrarySectionType field,
// falling back to the pre-built library map if the section type is not recognized.
func (s *Server) resolveMediaType(libraryID, sectionType string) media.MediaType {
	switch sectionType {
	case "movie":
		return media.MediaTypeMovie
	case "show":
		return media.MediaTypeTV
	}
	// Fall back to library map (built from Plex library list at startup)
	if info, ok := s.libraryMap[libraryID]; ok {
		return info.mediaType
	}
	return media.MediaTypeUnknown
}

func (s *Server) scheduleProcessing(libraryID, libraryName string, mediaType media.MediaType) {
	debounce := s.config.WebhookDebounce

	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()

	// Bump generation so any in-flight old callback becomes a no-op
	s.debounceGen[libraryID]++
	gen := s.debounceGen[libraryID]

	// Stop old timer if it exists (generation counter handles races)
	if timer, exists := s.debounceTimers[libraryID]; exists {
		timer.Stop()
		if s.config.VerboseLogging {
			fmt.Printf("Webhook: reset debounce timer for library %s\n", libraryName)
		}
	} else {
		fmt.Printf("Webhook: scheduled processing for library %s in %v\n", libraryName, debounce)
	}

	s.debounceTimers[libraryID] = time.AfterFunc(debounce, func() {
		s.debounceMu.Lock()
		if s.debounceGen[libraryID] != gen {
			s.debounceMu.Unlock()
			return
		}
		delete(s.debounceTimers, libraryID)
		delete(s.debounceGen, libraryID)
		s.debounceMu.Unlock()

		s.processLibrary(libraryID, libraryName, mediaType)
	})
}

func (s *Server) processLibrary(libraryID, libraryName string, mediaType media.MediaType) {
	fmt.Printf("Webhook: processing library %s (ID: %s)\n", libraryName, libraryID)
	if err := s.processor.ProcessAllItems(libraryID, libraryName, mediaType); err != nil {
		fmt.Printf("Webhook: error processing library %s: %v\n", libraryName, err)
	} else {
		fmt.Printf("Webhook: finished processing library %s\n", libraryName)
	}
}
