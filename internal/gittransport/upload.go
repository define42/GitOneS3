package gittransport

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/define42/GitOneS3/internal/repository"
)

// uploadNegotiation is per HTTP request or stateful SSH session. We advertise
// single-ACK only: the first common object is ACKed once, then flush/done are
// silent. Unknown haves are normal for unpublished local commits.
type uploadNegotiation struct {
	advertised   map[string]struct{}
	wants        map[string]string
	haves        map[string]string
	packets      int
	sideband     bool
	acknowledged bool
}

func newUploadNegotiation(snapshot *repository.GitSnapshot) *uploadNegotiation {
	n := &uploadNegotiation{
		advertised: make(map[string]struct{}, len(snapshot.References)),
		wants:      make(map[string]string),
		haves:      make(map[string]string),
	}
	for _, id := range snapshot.References {
		n.advertised[id] = struct{}{}
	}
	return n
}

func (n *uploadNegotiation) readPacket(ctx context.Context, reader *io.LimitedReader) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if n.packets >= maxNegotiationPackets {
		return nil, false, repository.ErrLimit
	}
	line, flush, err := readPkt(reader)
	if err != nil && reader.N == 0 {
		return nil, false, repository.ErrLimit
	}
	n.packets++ // Flushes also consume the bounded negotiation budget.
	return line, flush, err
}

func (n *uploadNegotiation) want(line []byte) error {
	fields := strings.Fields(string(line))
	if len(fields) < 2 || fields[0] != "want" || !validID(fields[1]) {
		return errPack
	}
	if _, ok := n.advertised[fields[1]]; !ok {
		return errPack
	}
	for _, capability := range fields[2:] {
		switch {
		case capability == "side-band-64k":
			n.sideband = true
		case capability == "ofs-delta", capability == "no-progress", strings.HasPrefix(capability, "agent="):
		default:
			return errPack
		}
	}
	// Synthetic tag roots retain the advertised object's actual kind, including
	// annotated tags and tags that target trees/blobs; root IDs deduplicate.
	n.wants["refs/tags/"+fields[1]] = fields[1]
	return nil
}

func (n *uploadNegotiation) have(snapshot *repository.GitSnapshot, fields []string) (string, error) {
	if len(fields) != 2 || fields[0] != "have" || !validID(fields[1]) {
		return "", errPack
	}
	id := fields[1]
	// LoadGitObjects limits Objects to the pinned, validated published graph.
	// Never consult another repository, a global object index, or orphan data.
	if _, ok := snapshot.Objects[id]; !ok {
		return "", nil
	}
	n.haves["refs/tags/"+id] = id
	if n.acknowledged {
		return "", nil
	}
	n.acknowledged = true
	return pkt("ACK " + id + "\n"), nil
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
	n := newUploadNegotiation(snapshot)
	bodyReader := bytes.NewReader(body)
	reader := &io.LimitedReader{R: bodyReader, N: maxNegotiationBytes}
	var output bytes.Buffer
	wantsDone, done := false, false
	for bodyReader.Len() > 0 {
		line, flush, err := n.readPacket(ctx, reader)
		if err != nil {
			return nil, err
		}
		if done {
			return nil, errPack
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
				return nil, err
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
			return nil, err
		}
		output.WriteString(ack)
	}
	if len(n.wants) == 0 {
		return nil, errPack
	}
	if !done {
		if output.Len() == 0 {
			output.WriteString(pkt("NAK\n"))
		}
		return output.Bytes(), nil
	}
	pack, err := n.pack(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	output.Write(pack)
	return output.Bytes(), nil
}
