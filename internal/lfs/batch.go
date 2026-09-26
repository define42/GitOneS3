package lfs

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/gittransport"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

type batchRequest struct {
	Operation string                 `json:"operation"`
	Transfers []string               `json:"transfers"`
	Objects   []repository.LFSObject `json:"objects"`
	HashAlgo  string                 `json:"hash_algo"`
}

type action struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
}
type objectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type batchObject struct {
	OID           string            `json:"oid"`
	Size          int64             `json:"size"`
	Authenticated bool              `json:"authenticated,omitempty"`
	Actions       map[string]action `json:"actions,omitempty"`
	Error         *objectError      `json:"error,omitempty"`
}
type batchResponse struct {
	Transfer string        `json:"transfer"`
	Objects  []batchObject `json:"objects"`
	HashAlgo string        `json:"hash_algo"`
}

func (h *Handler) batch(w http.ResponseWriter, r *http.Request, target route) {
	// Parse operation with the same case-sensitive semantics as authorization.
	// Unknown fields remain allowed for protocol extensions.
	var fields map[string]json.RawMessage
	if readJSON(w, r, &fields) != nil {
		writeError(w, http.StatusBadRequest, "invalid LFS batch")
		return
	}
	for key := range fields {
		if strings.EqualFold(key, "operation") && key != "operation" {
			writeError(w, http.StatusBadRequest, "invalid LFS operation key")
			return
		}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid LFS batch")
		return
	}
	var request batchRequest
	if json.Unmarshal(data, &request) != nil || (request.Operation != "upload" && request.Operation != "download") || request.Objects == nil {
		writeError(w, http.StatusBadRequest, "invalid LFS batch")
		return
	}
	if len(request.Objects) > maxBatchObjects {
		writeError(w, http.StatusRequestEntityTooLarge, "too many LFS objects")
		return
	}
	if (request.HashAlgo != "" && request.HashAlgo != "sha256") ||
		(len(request.Transfers) != 0 && !slices.Contains(request.Transfers, "basic")) {
		writeError(w, http.StatusUnprocessableEntity, "only basic transfers with sha256 are supported")
		return
	}
	if request.Operation == "upload" {
		authorize := gittransport.WriteAuthorization(r.Context())
		if authorize == nil {
			writeError(w, http.StatusForbidden, "write authorization required")
			return
		}
		if err := authorize(r.Context()); err != nil {
			writeError(w, http.StatusForbidden, "write permission revoked or expired")
			return
		}
	}
	response := batchResponse{Transfer: "basic", HashAlgo: "sha256", Objects: make([]batchObject, 0, len(request.Objects))}
	base := h.options.PublicURL + "/" + target.namespace + "/" + target.name + ".git/info/lfs/objects/"
	for _, requested := range request.Objects {
		if err := r.Context().Err(); err != nil {
			repositoryError(w, err)
			return
		}
		result := batchObject{OID: requested.OID, Size: requested.Size}
		switch {
		case !repository.ValidLFSOID(requested.OID) || requested.Size < 0:
			result.Error = &objectError{Code: http.StatusUnprocessableEntity, Message: "invalid object ID or size"}
		case request.Operation == "upload" && requested.Size > h.options.MaxObjectBytes:
			result.Error = &objectError{Code: http.StatusRequestEntityTooLarge, Message: "LFS object exceeds size limit"}
		default:
			object, err := h.store.LFSStat(r.Context(), target.namespace, target.name, requested.OID)
			switch {
			case err == nil:
				if object.Size != requested.Size {
					result.Error = &objectError{Code: http.StatusUnprocessableEntity, Message: "LFS object size mismatch"}
				} else if request.Operation == "download" {
					result.Actions = map[string]action{"download": {Href: base + requested.OID}}
				}
			case request.Operation == "upload" && (errors.Is(err, repository.ErrNotFound) || errors.Is(err, storage.ErrNotFound)):
				result.Actions = map[string]action{
					"upload": {Href: base + requested.OID + "?size=" + strconv.FormatInt(requested.Size, 10)},
					"verify": {Href: base + requested.OID + "/verify"},
				}
			default:
				status, message := repositoryStatus(err)
				result.Error = &objectError{Code: status, Message: message}
			}
		}
		// Carry SSH grants to our own action URLs without consulting a credential helper.
		if authorization := r.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") && len(result.Actions) > 0 {
			result.Authenticated = true
			// PUT already verifies the digest and size. Native LFS reuses the
			// action header for optional verification and cannot refresh an
			// SSH credential that expired during a successful long upload.
			delete(result.Actions, "verify")
			for name, next := range result.Actions {
				next.Header = map[string]string{"Authorization": authorization}
				if expires, ok := gittransport.LFSCredentialExpiry(r.Context()); ok {
					next.ExpiresAt = &expires
				}
				result.Actions[name] = next
			}
		}
		response.Objects = append(response.Objects, result)
	}
	writeJSON(w, http.StatusOK, response)
}
