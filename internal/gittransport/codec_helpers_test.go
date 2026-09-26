package gittransport

// Small in-memory test adapters exercise the same disk-backed codec and
// negotiation parser as the network handlers. Production has no buffered codec.
import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1" // #nosec G505 -- Git fixture checksums.
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"slices"

	"github.com/define42/GitOneS3/internal/gitpack"
	"github.com/define42/GitOneS3/internal/repository"
)

func codecError(err error) error {
	if errors.Is(err, gitpack.ErrLimit) {
		return errors.Join(repository.ErrLimit, err)
	}
	if errors.Is(err, gitpack.ErrInvalid) {
		return errors.Join(errPack, err)
	}
	return err
}
func decodePack(ctx context.Context, data []byte, existing map[string]repository.GitObject) (map[string]repository.GitObject, error) {
	reader := bytes.NewReader(data)
	workspace, err := gitpack.Decode(ctx, reader, repository.PackLimits(), func(_ context.Context, id string) (gitpack.Object, error) {
		object, ok := existing[id]
		if !ok {
			return gitpack.Object{}, gitpack.ErrNotFound
		}
		return gitpack.Object{Type: object.Type, Data: object.Data}, nil
	})
	if err != nil {
		return nil, codecError(err)
	}
	defer func() { _ = workspace.Close() }()
	if reader.Len() != 0 {
		return nil, errPack
	}
	result := map[string]repository.GitObject{}
	for _, id := range workspace.IDs() {
		object, err := workspace.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		result[id] = repository.GitObject{Type: object.Type, Data: object.Data}
	}
	return result, nil
}
func encodePack(ctx context.Context, objects map[string]repository.GitObject) ([]byte, error) {
	var out bytes.Buffer
	_, err := gitpack.Write(ctx, &out, slices.Collect(maps.Keys(objects)), func(_ context.Context, id string) (gitpack.Object, error) {
		object := objects[id]
		return gitpack.Object{Type: object.Type, Data: object.Data}, nil
	}, repository.PackLimits())
	if err != nil {
		return nil, codecError(err)
	}
	return out.Bytes(), nil
}
func applyDelta(base, delta []byte) ([]byte, error) {
	object := repository.GitObject{Type: "blob", Data: base}
	id := repository.GitObjectID(object)
	hash, _ := hex.DecodeString(id)
	data := append([]byte("PACK"), 0, 0, 0, 2, 0, 0, 0, 1)
	size := len(delta)
	b := byte(7<<4) | byte(size&15)
	size >>= 4
	for size > 0 {
		data = append(data, b|128)
		b = byte(size & 127)
		size >>= 7
	}
	data = append(data, b)
	data = append(data, hash...)
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(delta); err != nil {
		return nil, errors.Join(err, zw.Close())
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	data = append(data, compressed.Bytes()...)
	sum := sha1.Sum(data) // #nosec G401 -- Git fixture checksum.
	data = append(data, sum[:]...)
	objects, err := decodePack(context.Background(), data, map[string]repository.GitObject{id: object})
	if err != nil {
		return nil, err
	}
	for _, result := range objects {
		return result.Data, nil
	}
	return nil, errPack
}

type packCapture struct {
	reader io.Reader
	data   bytes.Buffer
}

func (r *packCapture) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.data.Write(p[:n])
	return n, err
}
func (r *packCapture) ReadByte() (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r, b[:])
	return b[0], err
}
func readStreamPack(ctx context.Context, reader *packCapture) error {
	workspace, err := gitpack.Decode(ctx, reader, repository.PackLimits(), nil)
	if err != nil {
		return codecError(err)
	}
	return workspace.Close()
}
func (n *uploadNegotiation) pack(ctx context.Context, snapshot *repository.GitSnapshot) ([]byte, error) {
	wanted, err := repository.ReachableGit(ctx, n.wants, snapshot.Objects)
	if err != nil {
		return nil, err
	}
	if len(n.haves) != 0 {
		known, err := repository.ReachableGit(ctx, n.haves, snapshot.Objects)
		if err != nil {
			return nil, err
		}
		// Validate complete graphs before subtraction. The incremental set is
		// deliberately incomplete: omitted parents/trees already exist locally.
		for id := range known {
			delete(wanted, id)
		}
	}
	pack, err := encodePack(ctx, wanted)
	if err != nil || !n.sideband {
		return pack, err
	}
	var output bytes.Buffer
	for len(pack) > 0 {
		size := min(len(pack), 65515)
		output.WriteString(pkt("\x01" + string(pack[:size])))
		pack = pack[size:]
	}
	output.WriteString("0000")
	return output.Bytes(), nil
}

func (h *Handler) upload(ctx context.Context, snapshot *repository.GitSnapshot, body []byte) ([]byte, error) {
	if len(body) > maxNegotiationBytes {
		return nil, repository.ErrLimit
	}
	n, prelude, done, err := parseUpload(ctx, snapshot, body)
	if err != nil {
		return nil, err
	}
	if !done {
		return prelude, nil
	}
	pack, err := n.pack(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	return append(prelude, pack...), nil
}
func (h *Handler) receive(ctx context.Context, snap *repository.GitSnapshot, body []byte) ([]byte, error) {
	r := bytes.NewReader(body)
	updates, err := receiveCommands(r)
	if err != nil {
		return nil, err
	}
	incoming := map[string]repository.GitObject{}
	if r.Len() > 0 {
		incoming, err = decodePack(ctx, body[len(body)-r.Len():], snap.Objects)
		if err != nil {
			//lint:ignore nilerr Git reports unpack failures in report-status, not as an HTTP transport error.
			return receiveStatus(updates, "invalid pack", "unpack failed"), nil
		}
	}
	authorize, _ := ctx.Value(writeAuthorizationKey{}).(func(context.Context) error)
	err = h.store.PublishGit(ctx, snap, updates, incoming, authorize)
	if err != nil {
		reason := "repository update failed"
		if errors.Is(err, repository.ErrConflict) {
			reason = "stale reference; fetch and retry"
		}
		if errors.Is(err, repository.ErrForbidden) {
			reason = "write permission revoked or expired"
		}
		if errors.Is(err, repository.ErrInvalid) {
			reason = "invalid reference or missing object"
		}
		if errors.Is(err, repository.ErrLimit) {
			reason = "repository exceeds limits"
		}
		return receiveStatus(updates, "ok", reason), nil
	}
	return receiveStatus(updates, "ok", ""), nil
}
