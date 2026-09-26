package gittransport

import (
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
	if !snapshot.HasObject(id) {
		return "", nil
	}
	n.haves["refs/tags/"+id] = id
	if n.acknowledged {
		return "", nil
	}
	n.acknowledged = true
	return pkt("ACK " + id + "\n"), nil
}
