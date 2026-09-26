package lfs

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

func (h *Handler) download(w http.ResponseWriter, r *http.Request, target route) {
	object, err := h.store.LFSStat(r.Context(), target.namespace, target.name, target.oid)
	if err != nil {
		repositoryError(w, err)
		return
	}
	etag := `"` + object.OID + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	if matchETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	offset, length, status := int64(0), object.Size, http.StatusOK
	rangeValue := r.Header.Get("Range")
	// Range applies to GET; HEAD reports the complete object's metadata.
	if rangeValue != "" && r.Method == http.MethodGet && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		offset, length, err = parseRange(rangeValue, object.Size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", object.Size))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid or unsupported byte range")
			return
		}
		status = http.StatusPartialContent
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	requestedLength := int64(-1)
	if status == http.StatusPartialContent {
		requestedLength = length
	}
	body, opened, err := h.store.LFSOpen(r.Context(), target.namespace, target.name, target.oid, offset, requestedLength)
	if err != nil {
		repositoryError(w, err)
		return
	}
	defer func() {
		if err := body.Close(); err != nil {
			slog.WarnContext(r.Context(), "close LFS download", "error", err)
		}
	}()
	if opened.OID != object.OID || opened.Size != object.Size {
		writeError(w, http.StatusServiceUnavailable, "LFS object changed")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, object.Size))
	}
	w.WriteHeader(status)
	if _, err := io.CopyN(w, body, length); err != nil {
		// Content-Length lets the client detect a truncated response. Abort the
		// stream so HTTP/2 clients also receive a reset on a failed upstream read.
		slog.WarnContext(r.Context(), "stream LFS download", "error", err)
		panic(http.ErrAbortHandler)
	}
}

func matchETag(value, etag string) bool {
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

// parseRange supports one inclusive, open-ended, or suffix byte range.
func parseRange(value string, size int64) (int64, int64, error) {
	invalid := errors.New("lfs: invalid byte range")
	if size <= 0 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, invalid
	}
	start, end, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok {
		return 0, 0, invalid
	}
	parse := func(s string) (int64, error) {
		if s == "" {
			return 0, invalid
		}
		for _, b := range s {
			if b < '0' || b > '9' {
				return 0, invalid
			}
		}
		return strconv.ParseInt(s, 10, 64)
	}
	if start == "" {
		n, err := parse(end)
		if err != nil || n <= 0 {
			return 0, 0, invalid
		}
		n = min(n, size)
		return size - n, n, nil
	}
	first, err := parse(start)
	if err != nil || first >= size {
		return 0, 0, invalid
	}
	last := size - 1
	if end != "" {
		last, err = parse(end)
		if err != nil || last < first {
			return 0, 0, invalid
		}
		last = min(last, size-1)
	}
	return first, last - first + 1, nil
}
