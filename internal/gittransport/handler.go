// Package gittransport serves bounded Git HTTP and SSH protocol v0 from S3-backed
// generations. It has no local working tree, hooks, subprocesses, or Git config.
package gittransport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
)

const zeroID = "0000000000000000000000000000000000000000"
const upload = "git-upload-pack"
const receive = "git-receive-pack"

type writeAuthorizationKey struct{}

// WithWriteAuthorization records the owning shard's token/membership recheck.
// It must not trust client headers; PublishGit invokes it after durable uploads.
func WithWriteAuthorization(ctx context.Context, authorize func(context.Context) error) context.Context {
	return context.WithValue(ctx, writeAuthorizationKey{}, authorize)
}

// WriteAuthorization returns the live permission check attached by the owning
// shard. A nil callback does not authorize writes.
func WriteAuthorization(ctx context.Context) func(context.Context) error {
	authorize, _ := ctx.Value(writeAuthorizationKey{}).(func(context.Context) error)
	return authorize
}

type Handler struct {
	store      *repository.Store
	operations *admission
	references *admission
}

func New(store *repository.Store, options ...Options) (*Handler, error) {
	if store == nil {
		return nil, errors.New("gittransport: repository store is required")
	}
	opts, err := admissionOptions(options)
	if err != nil {
		return nil, err
	}
	return &Handler{
		store:      store,
		operations: newAdmission(opts.MaxConcurrentOperations, opts.MaxQueuedOperations, opts.QueueTimeout),
		// Ref-only HTTP requests read bounded metadata, not object bodies. They
		// have their own small gate so transfers cannot monopolize discovery.
		references: newAdmission(4, opts.MaxQueuedOperations, opts.QueueTimeout),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline() // WithTimeout always supplies a deadline.
	controller := http.NewResponseController(w)
	defer func() {
		// An uncleared deadline only shortens connection life, but report failed
		// cleanup so wrappers or connection errors do not go unnoticed.
		for _, clear := range []func(time.Time) error{controller.SetReadDeadline, controller.SetWriteDeadline} {
			if err := clear(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
				slog.Warn("clear git transport deadline", "error", err)
			}
		}
	}()
	readErr := controller.SetReadDeadline(deadline)
	writeErr := controller.SetWriteDeadline(deadline)
	if (readErr != nil && !errors.Is(readErr, http.ErrNotSupported)) ||
		(writeErr != nil && !errors.Is(writeErr, http.ErrNotSupported)) {
		http.Error(w, "cannot establish git request deadline", http.StatusServiceUnavailable)
		return
	}
	namespace, name, service, advertise, err := parseRoute(r)
	if err != nil {
		http.Error(w, "invalid git request", http.StatusBadRequest)
		return
	}
	if !advertise {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/x-"+service+"-request" || r.Header.Get("Content-Encoding") != "" {
			http.Error(w, "unsupported git request type", http.StatusUnsupportedMediaType)
			return
		}
		if service == receive {
			authorize, _ := ctx.Value(writeAuthorizationKey{}).(func(context.Context) error)
			if authorize == nil {
				http.Error(w, "write authorization required", http.StatusForbidden)
				return
			}
		}
	}
	gate := h.operations
	if advertise {
		gate = h.references
	}
	release, err := gate.acquire(ctx)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "git service is busy; retry shortly", http.StatusServiceUnavailable)
		return
	}
	defer release()
	snapshot, err := h.store.ReadGitReferences(ctx, namespace, name)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, repository.ErrNotFound) {
			status = http.StatusNotFound
		}
		if errors.Is(err, repository.ErrInvalid) {
			status = http.StatusBadRequest
		}
		http.Error(w, "repository unavailable", status)
		return
	}
	if advertise {
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		writeBytes(w, advertisement(snapshot, service))
		return
	}
	if service == upload {
		body, readErr := io.ReadAll(io.LimitReader(r.Body, maxNegotiationBytes+1))
		if readErr != nil || len(body) > maxNegotiationBytes {
			http.Error(w, "invalid negotiation", http.StatusBadRequest)
			return
		}
		file, prepareErr := h.prepareUpload(ctx, snapshot, body)
		if prepareErr != nil {
			http.Error(w, "invalid or unavailable Git objects", http.StatusBadRequest)
			return
		}
		defer func() {
			// #nosec G703 -- prepareUpload creates this file with os.CreateTemp; its name is never supplied by the request.
			if err := errors.Join(file.Close(), os.Remove(file.Name())); err != nil {
				slog.WarnContext(ctx, "remove fetch workspace", "error", err)
			}
		}()
		w.Header().Set("Content-Type", "application/x-"+service+"-result")
		if _, err := io.Copy(w, file); err != nil {
			return
		}
		return
	}
	result, err := h.receiveStream(ctx, snapshot, r.Body, false)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, repository.ErrLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "invalid or unsupported git request", status)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-result")
	writeBytes(w, result)
}

func parseRoute(r *http.Request) (namespace, name, service string, advertise bool, err error) {
	invalid := errors.New("invalid route")
	if r == nil || r.URL == nil || r.URL.Opaque != "" || r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path {
		err = invalid
		return
	}
	path := r.URL.Path
	if r.RequestURI != "" {
		target, _, _ := strings.Cut(r.RequestURI, "?")
		if target != path {
			err = invalid
			return
		}
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if !strings.HasPrefix(path, "/") || (len(parts) != 3 && len(parts) != 4) || strings.ContainsAny(path, "\\%#") {
		err = invalid
		return
	}
	namespace = parts[0]
	name = strings.TrimSuffix(parts[1], ".git")
	if !strings.HasSuffix(parts[1], ".git") || !repository.ValidName(name) {
		err = invalid
		return
	}
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	if queryErr != nil {
		err = invalid
		return
	}
	if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" && r.Method == http.MethodGet {
		if len(query) != 1 || len(query["service"]) != 1 {
			err = invalid
			return
		}
		service = query.Get("service")
		advertise = true
	} else if len(parts) == 3 && r.Method == http.MethodPost && len(query) == 0 {
		service = parts[2]
	} else {
		err = invalid
		return
	}
	if service != upload && service != receive {
		err = invalid
	}
	return
}

func pkt(data string) string { return fmt.Sprintf("%04x%s", len(data)+4, data) }

func readPkt(r io.Reader) ([]byte, bool, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, false, err
	}
	for _, b := range header {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
			return nil, false, errPack
		}
	}
	n, err := strconv.ParseUint(string(header[:]), 16, 16)
	if err != nil {
		return nil, false, errPack
	}
	if n == 0 {
		return nil, true, nil
	}
	if n < 4 || n > 65520 {
		return nil, false, errPack
	}
	data := make([]byte, int(n)-4)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, false, err
	}
	return data, false, nil
}

func advertisement(snap *repository.GitSnapshot, service string) []byte {
	prefix := []byte(pkt("# service="+service+"\n") + "0000")
	return append(prefix, referenceAdvertisement(snap, service)...)
}

func referenceAdvertisement(snap *repository.GitSnapshot, service string) []byte {
	caps := "ofs-delta side-band-64k no-progress"
	if service == receive {
		caps = "report-status delete-refs ofs-delta atomic"
	} else {
		caps += " symref=HEAD:refs/heads/" + snap.DefaultBranch
	}
	refs := make([]string, 0, len(snap.References))
	for ref := range snap.References {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	var out strings.Builder
	first := true
	add := func(id, ref string) {
		line := id + " " + ref
		if first {
			line += "\x00" + caps
			first = false
		}
		out.WriteString(pkt(line + "\n"))
	}
	if service == upload {
		if head := snap.References["refs/heads/"+snap.DefaultBranch]; head != "" {
			add(head, "HEAD")
		}
	}
	for _, ref := range refs {
		add(snap.References[ref], ref)
	}
	if first {
		add(zeroID, "capabilities^{}")
	}
	out.WriteString("0000")
	return []byte(out.String())
}

func receiveCommands(r io.Reader) ([]repository.RefUpdate, error) {
	updates := []repository.RefUpdate{}
	first := true
	for {
		line, flush, err := readPkt(r)
		if err != nil {
			return nil, err
		}
		if flush {
			break
		}
		text := strings.TrimSuffix(string(line), "\n")
		if first {
			command, caps, _ := strings.Cut(text, "\x00")
			text = command
			for capability := range strings.FieldsSeq(caps) {
				if capability != "report-status" && capability != "delete-refs" && capability != "ofs-delta" && capability != "atomic" && !strings.HasPrefix(capability, "agent=") {
					return nil, errPack
				}
			}
			first = false
		}
		fields := strings.Split(text, " ")
		if len(fields) != 3 || !validID(fields[0]) || !validID(fields[1]) || !repository.ValidRef(fields[2]) || len(updates) >= 1000 {
			return nil, errPack
		}
		old, newID := fields[0], fields[1]
		if old == zeroID {
			old = ""
		}
		if newID == zeroID {
			newID = ""
		}
		updates = append(updates, repository.RefUpdate{Name: fields[2], Old: old, New: newID})
	}
	if len(updates) == 0 {
		return nil, errPack
	}
	return updates, nil
}

func receiveStatus(updates []repository.RefUpdate, unpack, reason string) []byte {
	var output strings.Builder
	output.WriteString(pkt("unpack " + unpack + "\n"))
	for _, update := range updates {
		if reason == "" {
			output.WriteString(pkt("ok " + update.Name + "\n"))
		} else {
			output.WriteString(pkt("ng " + update.Name + " " + reason + "\n"))
		}
	}
	output.WriteString("0000")
	return []byte(output.String())
}

func validID(id string) bool {
	if len(id) != 40 {
		return false
	}
	for _, b := range []byte(id) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func writeBytes(w http.ResponseWriter, data []byte) {
	// #nosec G705 -- Both callers set Git's binary media type and nosniff; this response is never HTML.
	if _, err := w.Write(data); err != nil {
		return
	}
}
