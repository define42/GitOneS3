// Package lfs serves Git LFS over HTTP through the owning GitOne shard.
// Object bytes always pass through GitOne; clients never receive storage URLs.
package lfs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

const mediaType = "application/vnd.git-lfs+json"
const maxJSONBytes = 1 << 20
const maxBatchObjects = 1000

// Options bounds LFS transfers separately from Git pack operations.
type Options struct {
	PublicURL              string
	MaxObjectBytes         int64
	MaxRepositoryBytes     int64
	MaxConcurrentTransfers int
	MaxQueuedTransfers     int
	QueueTimeout           time.Duration
	TransferTimeout        time.Duration
}

// Handler serves the basic Git LFS transfer adapter. Authentication runs before
// this handler; mutations additionally require a live authorization callback.
type Handler struct {
	store   *repository.Store
	options Options
	active  chan struct{}
	waiting chan struct{}
	control chan struct{}
}

func New(store *repository.Store, options Options) (*Handler, error) {
	if store == nil {
		return nil, errors.New("lfs: repository store is required")
	}
	u, err := url.Parse(options.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("lfs: public URL must be an HTTP(S) origin")
	}
	options.PublicURL = strings.TrimSuffix(options.PublicURL, "/")
	if options.MaxObjectBytes == 0 {
		options.MaxObjectBytes = 1 << 30
	}
	if options.MaxRepositoryBytes == 0 {
		options.MaxRepositoryBytes = 10 << 30
	}
	if options.MaxConcurrentTransfers == 0 {
		options.MaxConcurrentTransfers = 4
	}
	if options.QueueTimeout == 0 {
		options.QueueTimeout = 5 * time.Second
	}
	if options.TransferTimeout == 0 {
		options.TransferTimeout = 30 * time.Minute
	}
	if options.MaxObjectBytes < 1 || options.MaxObjectBytes > 64<<30 ||
		options.MaxRepositoryBytes < options.MaxObjectBytes || options.MaxRepositoryBytes > 1<<50 ||
		options.MaxConcurrentTransfers < 1 || options.MaxConcurrentTransfers > 32 ||
		options.MaxQueuedTransfers < 0 || options.MaxQueuedTransfers > 1024 ||
		options.QueueTimeout <= 0 || options.QueueTimeout > 90*time.Second ||
		options.TransferTimeout <= 0 || options.TransferTimeout > 12*time.Hour {
		return nil, errors.New("lfs: invalid transfer limits")
	}
	return &Handler{store: store, options: options, active: make(chan struct{}, options.MaxConcurrentTransfers),
		waiting: make(chan struct{}, options.MaxQueuedTransfers), control: make(chan struct{}, 8)}, nil
}

type route struct{ namespace, name, action, oid string }

func parseRoute(r *http.Request) (route, error) {
	if r.URL == nil || r.URL.Opaque != "" || r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
		return route{}, repository.ErrInvalid
	}
	if r.RequestURI != "" {
		path, _, _ := strings.Cut(r.RequestURI, "?")
		if path != r.URL.Path {
			return route{}, repository.ErrInvalid
		}
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 5 || !strings.HasSuffix(parts[1], ".git") || parts[2] != "info" || parts[3] != "lfs" {
		return route{}, repository.ErrInvalid
	}
	result := route{namespace: parts[0], name: strings.TrimSuffix(parts[1], ".git")}
	if !repository.ValidName(result.name) {
		return route{}, repository.ErrInvalid
	}
	if parts[4] == "locks" && (len(parts) == 5 ||
		(len(parts) == 6 && parts[5] == "verify") ||
		(len(parts) == 7 && parts[5] != "" && parts[6] == "unlock")) {
		result.action = "locks"
		return result, nil
	}
	if parts[4] != "objects" || len(parts) < 6 {
		return route{}, repository.ErrInvalid
	}
	if len(parts) == 6 && parts[5] == "batch" {
		result.action = "batch"
		return result, nil
	}
	if !repository.ValidLFSOID(parts[5]) {
		return route{}, repository.ErrInvalid
	}
	result.oid = parts[5]
	switch {
	case len(parts) == 6:
		result.action = "object"
	case len(parts) == 7 && parts[6] == "verify":
		result.action = "verify"
	default:
		return route{}, repository.ErrInvalid
	}
	return result, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	target, err := parseRoute(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid LFS request")
		return
	}
	if r.Header.Get("Content-Encoding") != "" {
		writeError(w, http.StatusUnsupportedMediaType, "content encoding is unsupported")
		return
	}
	transfer := target.action == "object"
	timeout := 30 * time.Second
	if transfer {
		timeout = h.options.TransferTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	r = r.WithContext(ctx)
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	defer func() {
		// Keep a finite read deadline through net/http's unread-body drain.
		// The server wrapper restores its default after this handler returns.
		if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			slog.WarnContext(ctx, "clear LFS deadline", "error", err)
		}
	}()
	for _, set := range []func(time.Time) error{controller.SetReadDeadline, controller.SetWriteDeadline} {
		if err := set(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeError(w, http.StatusServiceUnavailable, "cannot establish LFS deadline")
			return
		}
	}
	if transfer {
		release, err := h.acquire(ctx)
		if err != nil {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "LFS transfers are busy; retry shortly")
			return
		}
		defer release()
	} else {
		select {
		case h.control <- struct{}{}:
			defer func() { <-h.control }()
		default:
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "LFS service is busy; retry shortly")
			return
		}
	}
	if _, err := h.store.LFSRepository(ctx, target.namespace, target.name); err != nil {
		repositoryError(w, err)
		return
	}
	switch target.action {
	case "batch":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		h.batch(w, r, target)
	case "verify":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		h.verify(w, r, target)
	case "object":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.download(w, r, target)
		case http.MethodPut:
			h.upload(w, r, target)
		default:
			methodNotAllowed(w, "GET, HEAD, PUT")
		}
	default:
		writeError(w, http.StatusNotImplemented, "LFS file locking is unavailable")
	}
}

func (h *Handler) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release := func() { <-h.active }
	select {
	case h.active <- struct{}{}:
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	default:
	}
	select {
	case h.waiting <- struct{}{}:
		defer func() { <-h.waiting }()
	default:
		return nil, errors.New("lfs: admission queue full")
	}
	wait, cancel := context.WithTimeout(ctx, h.options.QueueTimeout)
	defer cancel()
	select {
	case h.active <- struct{}{}:
		if err := wait.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	case <-wait.Done():
		return nil, wait.Err()
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, value any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != mediaType {
		return errors.New("unsupported LFS content type")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func methodNotAllowed(w http.ResponseWriter, methods string) {
	w.Header().Set("Allow", methods)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func repositoryStatus(err error) (int, string) {
	switch {
	case errors.Is(err, repository.ErrLFSQuota):
		return http.StatusInsufficientStorage, "LFS repository quota exceeded"
	case errors.Is(err, repository.ErrLFSHashMismatch):
		return http.StatusUnprocessableEntity, "LFS object hash or size mismatch"
	case errors.Is(err, repository.ErrNotFound), errors.Is(err, storage.ErrNotFound):
		return http.StatusNotFound, "LFS object or repository not found"
	case errors.Is(err, repository.ErrForbidden):
		return http.StatusForbidden, "write permission revoked or expired"
	case errors.Is(err, repository.ErrLimit):
		return http.StatusRequestEntityTooLarge, "LFS storage limit exceeded"
	case errors.Is(err, repository.ErrInvalid):
		return http.StatusUnprocessableEntity, "invalid LFS object or request"
	case errors.Is(err, repository.ErrConflict):
		return http.StatusConflict, "repository update in progress; retry"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "LFS operation timed out"
	default:
		return http.StatusServiceUnavailable, "LFS storage unavailable"
	}
}

func repositoryError(w http.ResponseWriter, err error) {
	status, message := repositoryStatus(err)
	writeError(w, status, message)
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request, target route) {
	authorize := gittransport.WriteAuthorization(r.Context())
	if authorize == nil {
		writeError(w, http.StatusForbidden, "write authorization required")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["size"]) != 1 {
		writeError(w, http.StatusBadRequest, "upload size is required")
		return
	}
	size, err := strconv.ParseInt(query.Get("size"), 10, 64)
	if err != nil || size < 0 || strconv.FormatInt(size, 10) != query.Get("size") {
		writeError(w, http.StatusBadRequest, "invalid upload size")
		return
	}
	if size > h.options.MaxObjectBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "LFS object exceeds size limit")
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != size {
		writeError(w, http.StatusUnprocessableEntity, "upload size does not match content length")
		return
	}
	// Git LFS may sniff a media type from the file. Every upload is opaque bytes.
	r.Body = http.MaxBytesReader(w, r.Body, size+1)
	_, err = h.store.LFSUpload(r.Context(), target.namespace, target.name, target.oid, size, r.Body,
		repository.LFSLimits{MaxObjectBytes: h.options.MaxObjectBytes, MaxRepositoryBytes: h.options.MaxRepositoryBytes}, authorize)
	if err != nil {
		repositoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request, target route) {
	authorize := gittransport.WriteAuthorization(r.Context())
	if authorize == nil {
		writeError(w, http.StatusForbidden, "write authorization required")
		return
	}
	var requested repository.LFSObject
	if readJSON(w, r, &requested) != nil || requested.OID != target.oid || requested.Size < 0 {
		writeError(w, http.StatusBadRequest, "invalid verification request")
		return
	}
	if err := authorize(r.Context()); err != nil {
		writeError(w, http.StatusForbidden, "write permission revoked or expired")
		return
	}
	object, err := h.store.LFSStat(r.Context(), target.namespace, target.name, target.oid)
	if err != nil {
		repositoryError(w, err)
		return
	}
	if object.Size != requested.Size {
		writeError(w, http.StatusUnprocessableEntity, "LFS object size mismatch")
		return
	}
	w.WriteHeader(http.StatusOK)
}
