package gittransport

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/define42/GitOneS3/internal/repository"
)

const maxNegotiationBytes = 1 << 20
const maxNegotiationPackets = 20000

// SSHRequest describes an already authenticated and authorized Git session.
// Stream carries Git wire bytes, not SSH framing. Its owner must interrupt
// blocked I/O when ctx is canceled and enforce a maximum 90-second I/O deadline.
type SSHRequest struct {
	Namespace  string
	Repository string
	Service    string
	Stream     io.ReadWriter
}

// ServeSSH serves a single Git-only exec session. HTTP and SSH share admission,
// object limits, snapshots, pack validation, and the final write authorization.
func (h *Handler) ServeSSH(ctx context.Context, request SSHRequest) error {
	if request.Stream == nil || (request.Service != upload && request.Service != receive) {
		return errors.New("gittransport: invalid ssh request")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("git ssh session: %w", err)
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		return errors.New("gittransport: git service is busy; retry shortly")
	}
	if request.Service == receive {
		authorize, _ := ctx.Value(writeAuthorizationKey{}).(func(context.Context) error)
		if authorize == nil {
			return repository.ErrForbidden
		}
	}
	snapshot, err := h.store.ReadGit(ctx, request.Namespace, request.Repository)
	if err != nil {
		return fmt.Errorf("load git ssh repository: %w", err)
	}
	if err := writeSSH(request.Stream, referenceAdvertisement(snapshot, request.Service)); err != nil {
		return err
	}
	if request.Service == upload {
		return h.uploadSSH(ctx, snapshot, request.Stream)
	}
	return h.receiveSSH(ctx, snapshot, request.Stream)
}

func (h *Handler) uploadSSH(ctx context.Context, snapshot *repository.GitSnapshot, stream io.ReadWriter) error {
	reader := &io.LimitedReader{R: stream, N: maxNegotiationBytes}
	var wants bytes.Buffer
	count := 0
	for {
		line, flush, err := readPkt(reader)
		if errors.Is(err, io.EOF) && wants.Len() == 0 {
			return nil // ls-remote may close after reading the advertisement.
		}
		if err != nil {
			return fmt.Errorf("read ssh wants: %w", err)
		}
		count++
		if count > maxNegotiationPackets {
			return repository.ErrLimit
		}
		if flush {
			break
		}
		fields := strings.Fields(string(line))
		if len(fields) == 0 || fields[0] != "want" {
			return errPack
		}
		wants.WriteString(pkt(string(line)))
	}
	if wants.Len() == 0 {
		return nil // No requested objects, including an up-to-date fetch.
	}
	// Reuse HTTP's capability and advertised-object checks. The initial wants
	// flush has no response in the stateful protocol; only have rounds get NAK.
	if _, err := h.upload(ctx, snapshot, wants.Bytes()); err != nil {
		return fmt.Errorf("validate ssh wants: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("negotiate ssh fetch: %w", err)
		}
		line, flush, err := readPkt(reader)
		if err != nil {
			return fmt.Errorf("read ssh negotiation: %w", err)
		}
		count++
		if count > maxNegotiationPackets {
			return repository.ErrLimit
		}
		if flush {
			if err := writeSSH(stream, []byte(pkt("NAK\n"))); err != nil {
				return err
			}
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) == 2 && fields[0] == "have" && validID(fields[1]) {
			continue
		}
		if len(fields) != 1 || fields[0] != "done" {
			return errPack
		}
		wants.WriteString(pkt("done\n"))
		result, err := h.upload(ctx, snapshot, wants.Bytes())
		if err != nil {
			return fmt.Errorf("prepare ssh fetch: %w", err)
		}
		return writeSSH(stream, result)
	}
}

func (h *Handler) receiveSSH(ctx context.Context, snapshot *repository.GitSnapshot, stream io.ReadWriter) error {
	reader := &packCapture{reader: stream}
	for count := 0; ; count++ {
		_, flush, err := readPkt(reader)
		if err != nil {
			return fmt.Errorf("read ssh reference updates: %w", err)
		}
		if flush {
			if count == 0 {
				return nil // Nothing to push.
			}
			break
		}
		if count >= 1000 || reader.data.Len() > maxNegotiationBytes {
			return repository.ErrLimit
		}
	}
	updates, err := receiveCommands(bytes.NewReader(reader.data.Bytes()))
	if err != nil {
		return fmt.Errorf("validate ssh reference updates: %w", err)
	}
	for _, update := range updates {
		if update.New == "" {
			continue
		}
		// A create/update always carries a pack, even if its object count is
		// zero. Delete-only pushes have no pack and must not wait for EOF.
		if err := readStreamPack(ctx, reader); err != nil {
			return fmt.Errorf("read ssh pack: %w", err)
		}
		break
	}
	result, err := h.receive(ctx, snapshot, reader.data.Bytes())
	if err != nil {
		return fmt.Errorf("apply ssh push: %w", err)
	}
	return writeSSH(stream, result)
}

// packCapture implements io.ByteReader so zlib never reads past an object's
// compressed stream. Capturing only consumed bytes makes a PACK self-delimiting:
// the peer can keep stdin open while waiting for receive-pack's report-status.
type packCapture struct {
	reader io.Reader
	data   bytes.Buffer
}

func (r *packCapture) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := maxPackBytes - r.data.Len()
	if remaining <= 0 {
		return 0, repository.ErrLimit
	}
	n, err := r.reader.Read(p[:min(len(p), remaining)])
	r.data.Write(p[:n])
	return n, err
}

func (r *packCapture) ReadByte() (byte, error) {
	var data [1]byte
	_, err := io.ReadFull(r, data[:])
	return data[0], err
}

// readStreamPack bounds all advertised/decompressed sizes while locating the
// checksum. decodePack subsequently verifies checksums, deltas and object IDs.
func readStreamPack(ctx context.Context, reader *packCapture) error {
	var header [12]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return fmt.Errorf("read pack header: %w", err)
	}
	version := binary.BigEndian.Uint32(header[4:8])
	count := binary.BigEndian.Uint32(header[8:12])
	if string(header[:4]) != "PACK" || (version != 2 && version != 3) {
		return errPack
	}
	if count > repository.MaxGitObjects {
		return repository.ErrLimit
	}
	var total int64
	for range count {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("read pack object: %w", err)
		}
		size, err := streamObjectHeader(reader)
		if err != nil {
			return err
		}
		total += size
		if total > repository.MaxGitBytes {
			return repository.ErrLimit
		}
		compressed, err := zlib.NewReader(reader)
		if err != nil {
			return fmt.Errorf("open compressed object: %w", err)
		}
		n, readErr := io.Copy(io.Discard, io.LimitReader(compressed, size+1))
		closeErr := compressed.Close()
		if readErr != nil || closeErr != nil || n != size {
			return errors.Join(errPack, readErr, closeErr)
		}
	}
	var checksum [20]byte
	if _, err := io.ReadFull(reader, checksum[:]); err != nil {
		return fmt.Errorf("read pack checksum: %w", err)
	}
	return nil
}

func streamObjectHeader(reader *packCapture) (int64, error) {
	first, err := reader.ReadByte()
	if err != nil {
		return 0, fmt.Errorf("read object header: %w", err)
	}
	size := uint64(first & 15)
	last := first
	for shift := uint(4); last&128 != 0; shift += 7 {
		if shift > 25 {
			return 0, errPack
		}
		last, err = reader.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("read object size: %w", err)
		}
		size |= uint64(last&127) << shift
	}
	if size > repository.MaxGitObjectBytes {
		return 0, repository.ErrLimit
	}
	switch (first >> 4) & 7 {
	case 1, 2, 3, 4:
	case 6:
		for n := 0; ; n++ {
			if n > 8 {
				return 0, errPack
			}
			b, err := reader.ReadByte()
			if err != nil {
				return 0, fmt.Errorf("read delta offset: %w", err)
			}
			if b&128 == 0 {
				break
			}
		}
	case 7:
		var base [20]byte
		if _, err := io.ReadFull(reader, base[:]); err != nil {
			return 0, fmt.Errorf("read delta base: %w", err)
		}
	default:
		return 0, errPack
	}
	return int64(size), nil // #nosec G115 -- The advertised size is bounded to MaxGitObjectBytes above.
}

func writeSSH(stream io.Writer, data []byte) error {
	n, err := stream.Write(data)
	if err != nil {
		return fmt.Errorf("write git ssh response: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("write git ssh response: %w", io.ErrShortWrite)
	}
	return nil
}
