package gittransport

import (
	"context"
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
	if request.Service == receive {
		authorize, _ := ctx.Value(writeAuthorizationKey{}).(func(context.Context) error)
		if authorize == nil {
			return repository.ErrForbidden
		}
	}
	release, err := h.operations.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	snapshot, err := h.store.ReadGitReferences(ctx, request.Namespace, request.Repository)
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

func (h *Handler) uploadSSH(ctx context.Context, snapshot *repository.GitSnapshot, stream io.ReadWriter) (err error) {
	reader := &io.LimitedReader{R: stream, N: maxNegotiationBytes}
	n := newUploadNegotiation(snapshot)
	for {
		line, flush, err := n.readPacket(ctx, reader)
		if errors.Is(err, io.EOF) && len(n.wants) == 0 {
			return nil // ls-remote may close after reading the advertisement.
		}
		if err != nil {
			return fmt.Errorf("read ssh wants: %w", err)
		}
		if flush {
			break
		}
		if err := n.want(line); err != nil {
			return fmt.Errorf("validate ssh wants: %w", err)
		}
	}
	if len(n.wants) == 0 {
		return nil // No requested objects, including an up-to-date fetch.
	}
	objectReader, err := h.store.OpenGit(ctx, snapshot)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, objectReader.Close()) }()
	if err := objectReader.Validate(ctx); err != nil {
		return err
	}
	for {
		line, flush, err := n.readPacket(ctx, reader)
		if err != nil {
			return fmt.Errorf("read ssh negotiation: %w", err)
		}
		if flush {
			if !n.acknowledged {
				if err := writeSSH(stream, []byte(pkt("NAK\n"))); err != nil {
					return err
				}
			}
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) != 1 || fields[0] != "done" {
			ack, err := n.have(snapshot, fields)
			if err != nil {
				return err
			}
			if ack != "" {
				if err := writeSSH(stream, []byte(ack)); err != nil {
					return err
				}
			}
			continue
		}
		if !n.acknowledged {
			if err := writeSSH(stream, []byte(pkt("NAK\n"))); err != nil {
				return err
			}
		}
		return n.writePack(ctx, snapshot, objectReader, stream)
	}
}

func (h *Handler) receiveSSH(ctx context.Context, snapshot *repository.GitSnapshot, stream io.ReadWriter) error {
	result, err := h.receiveStream(ctx, snapshot, stream, true)
	if err != nil {
		return err
	}
	return writeSSH(stream, result)
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
