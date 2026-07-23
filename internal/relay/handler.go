package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/presencev2"
	"github.com/swemonstro/aurora/internal/status"
)

const (
	maxSnapshotBodyBytes     = 8 * 1024
	maxPresentationBodyBytes = 16 * 1024
)

type Handler struct {
	store *Store
}

func NewHandler(store *Store) (*Handler, error) {
	if store == nil {
		return nil, fmt.Errorf("store must not be nil")
	}

	return &Handler{store: store}, nil
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/presence", h.handlePresence)
	mux.HandleFunc(
		"/presence/presentation",
		h.handlePresentation,
	)

	return mux
}

func (h *Handler) handlePresentation(
	w http.ResponseWriter,
	r *http.Request,
) {
	switch r.Method {
	case http.MethodGet:
		h.getPresentation(w)
	case http.MethodPost:
		h.postPresentation(w, r)
	case http.MethodDelete:
		h.store.RemovePresentation()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(
			w,
			http.StatusMethodNotAllowed,
			"method not allowed",
		)
	}
}

func (h *Handler) getPresentation(w http.ResponseWriter) {
	presentation, ok := h.store.Presentation()
	if !ok {
		writeError(
			w,
			http.StatusNotFound,
			"no presence presentation available",
		)
		return
	}

	writeJSON(w, http.StatusOK, presentation)
}

func (h *Handler) postPresentation(
	w http.ResponseWriter,
	r *http.Request,
) {
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			writeError(
				w,
				http.StatusUnsupportedMediaType,
				"content type must be application/json",
			)
			return
		}
	}

	body := http.MaxBytesReader(
		w,
		r.Body,
		maxPresentationBodyBytes,
	)
	defer body.Close()

	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	var presentation presencev2.Presentation
	if err := decoder.Decode(&presentation); err != nil {
		writeDecodeError(w, err)
		return
	}

	if err := ensureSingleJSONValue(decoder); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := presentation.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.store.SetPresentation(presentation)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handlePresence(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.getPresence(w)
	case http.MethodPost:
		h.postPresence(w, r)
	case http.MethodDelete:
		h.deletePresence(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) deletePresence(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		writeError(w, http.StatusBadRequest, "source must not be empty")
		return
	}

	h.store.Remove(source)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getPresence(w http.ResponseWriter) {
	snapshot, ok := h.store.Latest()
	if !ok {
		writeError(w, http.StatusNotFound, "no presence snapshot available")
		return
	}

	writeJSON(w, http.StatusOK, snapshot)
}

func (h *Handler) postPresence(w http.ResponseWriter, r *http.Request) {
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
			return
		}
	}

	body := http.MaxBytesReader(w, r.Body, maxSnapshotBodyBytes)
	defer body.Close()

	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	var snapshot presence.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		writeDecodeError(w, err)
		return
	}

	if err := ensureSingleJSONValue(decoder); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := validateSnapshot(snapshot); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.store.Set(snapshot)
	w.WriteHeader(http.StatusNoContent)
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var extra any

	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}

	return fmt.Errorf("request must contain exactly one JSON object")
}

func validateSnapshot(snapshot presence.Snapshot) error {
	if snapshot.Version != presence.ProtocolVersion {
		return fmt.Errorf(
			"unsupported protocol version %d",
			snapshot.Version,
		)
	}

	if strings.TrimSpace(snapshot.Source) == "" {
		return fmt.Errorf("source must not be empty")
	}

	switch snapshot.State {
	case status.Working, status.Attention, status.Error, status.Idle:
	default:
		return fmt.Errorf("unsupported state %q", snapshot.State)
	}

	if snapshot.Timestamp.IsZero() {
		return fmt.Errorf("timestamp must not be zero")
	}

	return nil
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}

	writeError(w, http.StatusBadRequest, "invalid JSON body")
}

func writeError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, struct {
		Error string `json:"error"`
	}{
		Error: message,
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	_ = json.NewEncoder(w).Encode(value)
}
