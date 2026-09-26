package gittransport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/define42/GitOneS3/internal/gitpack"
	"github.com/define42/GitOneS3/internal/repository"
)

// receiveStream reads only the small command section into memory. Pack bodies
// and resolved deltas are staged on disk by gitpack before immutable publication.
func (h *Handler) receiveStream(ctx context.Context, snap *repository.GitSnapshot, input io.Reader, ssh bool) (_ []byte, err error) {
	var r byteReader = exactReader{input}
	var buffered *bufio.Reader
	if !ssh {
		buffered = bufio.NewReader(input)
		r = buffered
	}
	var commands bytes.Buffer
	limited := &io.LimitedReader{R: r, N: maxNegotiationBytes}
	commandReader := io.TeeReader(limited, &commands)
	for count := 0; ; count++ {
		_, flush, readErr := readPkt(commandReader)
		if readErr != nil {
			if limited.N == 0 {
				return nil, repository.ErrLimit
			}
			return nil, readErr
		}
		if flush {
			if count == 0 {
				// Git probes HTTP receive-pack with a lone flush before a
				// large chunked POST. SSH uses the same no-update message.
				if !ssh {
					if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
						return nil, errPack
					}
				}
				return nil, nil
			}
			break
		}
		if count >= 1000 {
			return nil, repository.ErrLimit
		}
	}
	updates, err := receiveCommands(bytes.NewReader(commands.Bytes()))
	if err != nil {
		return nil, err
	}
	reader, err := h.store.OpenGit(ctx, snap)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	if err := reader.Validate(ctx); err != nil {
		return nil, err
	}
	hasPack := false
	if ssh {
		for _, update := range updates {
			if update.New != "" {
				hasPack = true
				break
			}
		}
	} else {
		_, peekErr := buffered.Peek(1)
		if peekErr != nil && !errors.Is(peekErr, io.EOF) {
			return nil, peekErr
		}
		hasPack = peekErr == nil
	}
	var incoming *gitpack.Workspace
	if hasPack {
		incoming, err = gitpack.Decode(ctx, r, repository.PackLimits(), func(ctx context.Context, id string) (gitpack.Object, error) {
			if !snap.HasObject(id) {
				return gitpack.Object{}, gitpack.ErrNotFound
			}
			return reader.Get(ctx, id)
		})
		if err != nil {
			// Invalid uploads use Git's report-status and never publish refs.
			return receiveStatus(updates, "invalid pack", "unpack failed"), nil
		}
		defer func() { err = errors.Join(err, incoming.Close()) }()
	}
	if !ssh {
		if _, trailingErr := r.ReadByte(); !errors.Is(trailingErr, io.EOF) {
			return receiveStatus(updates, "invalid pack", "unexpected trailing data"), nil
		}
	}
	if err := reader.Close(); err != nil {
		return nil, err
	}
	authorize, _ := ctx.Value(writeAuthorizationKey{}).(func(context.Context) error)
	err = h.store.PublishPack(ctx, snap, updates, incoming, authorize)
	return publicationStatus(updates, err), nil
}

type byteReader interface {
	io.Reader
	io.ByteReader
}
type exactReader struct{ io.Reader }

func (r exactReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r.Reader, b[:])
	return b[0], err
}

func publicationStatus(updates []repository.RefUpdate, err error) []byte {
	reason := ""
	if err != nil {
		reason = "repository update failed"
		switch {
		case errors.Is(err, repository.ErrMaintenanceBusy):
			reason = "repository maintenance or write in progress; retry"
		case errors.Is(err, repository.ErrConflict):
			reason = "stale reference; fetch and retry"
		case errors.Is(err, repository.ErrForbidden):
			reason = "write permission revoked or expired"
		case errors.Is(err, repository.ErrInvalid):
			reason = "invalid reference or missing object"
		case errors.Is(err, repository.ErrLimit), errors.Is(err, gitpack.ErrLimit):
			reason = "repository exceeds limits"
		}
	}
	return receiveStatus(updates, "ok", reason)
}

func (n *uploadNegotiation) writePack(ctx context.Context, snapshot *repository.GitSnapshot, reader *repository.GitReader, out io.Writer) error {
	wanted, err := reader.Reachable(ctx, n.wants)
	if err != nil {
		return err
	}
	if len(n.haves) != 0 {
		known, err := reader.Reachable(ctx, n.haves)
		if err != nil {
			return err
		}
		omit := make(map[string]bool, len(known))
		for _, id := range known {
			omit[id] = true
		}
		filtered := wanted[:0]
		for _, id := range wanted {
			if !omit[id] {
				filtered = append(filtered, id)
			}
		}
		wanted = filtered
	}
	packWriter := out
	if n.sideband {
		packWriter = sidebandWriter{out}
	}
	if _, err := gitpack.Write(ctx, packWriter, wanted, reader.Get, repository.PackLimits()); err != nil {
		return err
	}
	if n.sideband {
		return writeSSH(out, []byte("0000"))
	}
	return nil
}

type sidebandWriter struct{ writer io.Writer }

func (w sidebandWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		size := min(len(p), 65515)
		if err := writeSSH(w.writer, []byte(pkt("\x01"+string(p[:size])))); err != nil {
			return n, err
		}
		n += size
		p = p[size:]
	}
	return n, nil
}

// prepareUpload validates the bounded HTTP negotiation. A disk file carries the
// response so corruption and missing objects are reported before HTTP headers;
// the complete pack is never buffered in RAM.
func (h *Handler) prepareUpload(ctx context.Context, snapshot *repository.GitSnapshot, body []byte) (_ *os.File, err error) {
	if len(body) > maxNegotiationBytes {
		return nil, repository.ErrLimit
	}
	reader, err := h.store.OpenGit(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	var file *os.File
	defer func() {
		err = errors.Join(err, reader.Close())
		if err != nil && file != nil {
			err = errors.Join(err, file.Close(), os.Remove(file.Name()))
		}
	}()
	if err := reader.Validate(ctx); err != nil {
		return nil, err
	}
	n, prelude, done, err := parseUpload(ctx, snapshot, body)
	if err != nil {
		return nil, err
	}
	file, err = os.CreateTemp("", "gitone-fetch-*")
	if err != nil {
		return nil, err
	}
	if err := writeSSH(file, prelude); err != nil {
		return nil, err
	}
	if done {
		if err := n.writePack(ctx, snapshot, reader, file); err != nil {
			return nil, err
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return file, nil
}

func parseUpload(ctx context.Context, snapshot *repository.GitSnapshot, body []byte) (*uploadNegotiation, []byte, bool, error) {
	n := newUploadNegotiation(snapshot)
	bodyReader := bytes.NewReader(body)
	reader := &io.LimitedReader{R: bodyReader, N: maxNegotiationBytes}
	var output bytes.Buffer
	wantsDone, done := false, false
	for bodyReader.Len() > 0 {
		line, flush, err := n.readPacket(ctx, reader)
		if err != nil {
			return nil, nil, false, err
		}
		if done {
			return nil, nil, false, errPack
		}
		if flush {
			if !wantsDone {
				wantsDone = true
			} else if !n.acknowledged {
				output.WriteString(pkt("NAK\n"))
			}
			continue
		}
		if !wantsDone {
			if err := n.want(line); err != nil {
				return nil, nil, false, err
			}
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) == 1 && fields[0] == "done" {
			done = true
			if !n.acknowledged {
				output.WriteString(pkt("NAK\n"))
			}
			continue
		}
		ack, err := n.have(snapshot, fields)
		if err != nil {
			return nil, nil, false, err
		}
		output.WriteString(ack)
	}
	if len(n.wants) == 0 {
		return nil, nil, false, errPack
	}
	if !done && output.Len() == 0 {
		output.WriteString(pkt("NAK\n"))
	}
	return n, output.Bytes(), done, nil
}
