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

const eventLibraryNew = "library.new"

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

// pendingWork tracks accumulated rating keys during a debounce window.
type pendingWork struct {
	libraryName string
	mediaType   media.MediaType
	ratingKeys  []string
	timer       *time.Timer
	gen         uint64
}

type Server struct {
	config     *config.Config
	processor  *media.Processor
	httpServer *http.Server
	libraryMap map[string]libraryInfo
	pending    map[string]*pendingWork
	pendingMu  sync.Mutex
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
		config:     cfg,
		processor:  proc,
		libraryMap: libMap,
		pending:    make(map[string]*pendingWork),
	}
}

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

	if payload.Event != eventLibraryNew {
		w.WriteHeader(http.StatusOK)
		return
	}

	libraryID := strconv.Itoa(payload.Metadata.LibrarySectionID)
	mediaType := s.resolveMediaType(libraryID, payload.Metadata.LibrarySectionType)
	libraryName := payload.Metadata.LibrarySectionTitle
	if libraryName == "" {
		if info, ok := s.libraryMap[libraryID]; ok {
			libraryName = info.name
		}
	}

	if mediaType != media.MediaTypeUnknown {
		s.addPendingItem(libraryID, libraryName, mediaType, payload.Metadata.RatingKey)
	} else if s.config.VerboseLogging {
		fmt.Printf("Webhook: ignoring event for unknown library %s (ID: %s)\n", libraryName, libraryID)
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) resolveMediaType(libraryID, sectionType string) media.MediaType {
	switch sectionType {
	case "movie":
		return media.MediaTypeMovie
	case "show":
		return media.MediaTypeTV
	}
	if info, ok := s.libraryMap[libraryID]; ok {
		return info.mediaType
	}
	return media.MediaTypeUnknown
}

// addPendingItem accumulates rating keys during the debounce window. Each new
// event for the same library resets the timer and adds the rating key to the list.
// When the timer fires, all accumulated keys are processed.
func (s *Server) addPendingItem(libraryID, libraryName string, mediaType media.MediaType, ratingKey string) {
	debounce := s.config.WebhookDebounce

	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	pw, exists := s.pending[libraryID]
	if exists {
		pw.timer.Stop()
		pw.gen++
		if ratingKey != "" {
			pw.ratingKeys = appendUnique(pw.ratingKeys, ratingKey)
		}
		if s.config.VerboseLogging {
			fmt.Printf("Webhook: reset debounce for library %s (%d items queued)\n", libraryName, len(pw.ratingKeys))
		}
	} else {
		var keys []string
		if ratingKey != "" {
			keys = []string{ratingKey}
		}
		pw = &pendingWork{
			libraryName: libraryName,
			mediaType:   mediaType,
			ratingKeys:  keys,
		}
		s.pending[libraryID] = pw
		fmt.Printf("Webhook: scheduled processing for library %s in %v\n", libraryName, debounce)
	}

	gen := pw.gen
	pw.timer = time.AfterFunc(debounce, func() {
		s.pendingMu.Lock()
		current, ok := s.pending[libraryID]
		if !ok || current.gen != gen {
			s.pendingMu.Unlock()
			return
		}
		keys := current.ratingKeys
		delete(s.pending, libraryID)
		s.pendingMu.Unlock()

		s.processItems(libraryID, libraryName, mediaType, keys)
	})
}

func appendUnique(slice []string, val string) []string {
	for _, v := range slice {
		if v == val {
			return slice
		}
	}
	return append(slice, val)
}

func (s *Server) processItems(libraryID, libraryName string, mediaType media.MediaType, ratingKeys []string) {
	if len(ratingKeys) == 0 {
		fmt.Printf("Webhook: processing full library %s (no rating keys in events)\n", libraryName)
		if err := s.processor.ProcessAllItems(libraryID, libraryName, mediaType); err != nil {
			fmt.Printf("Webhook: error processing library %s: %v\n", libraryName, err)
		} else {
			fmt.Printf("Webhook: finished processing library %s\n", libraryName)
		}
		return
	}

	fmt.Printf("Webhook: processing %d items in library %s\n", len(ratingKeys), libraryName)
	for _, key := range ratingKeys {
		if err := s.processor.ProcessSingleItem(key, libraryID, mediaType); err != nil {
			fmt.Printf("Webhook: error processing item %s: %v\n", key, err)
		}
	}
	fmt.Printf("Webhook: finished processing %d items in library %s\n", len(ratingKeys), libraryName)
}
