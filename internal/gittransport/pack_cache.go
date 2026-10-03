package gittransport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"

	"github.com/define42/GitOneS3/internal/cache"
	"github.com/define42/GitOneS3/internal/gitpack"
	"github.com/define42/GitOneS3/internal/repository"
)

// cachedPack shares only complete requested histories. Incremental fetches have
// too many possible have sets to retain usefully. The exact selected object IDs
// are part of the key even for clones: different requested branches must never
// receive each other's objects. Negotiation and sideband bytes remain private.
func (n *uploadNegotiation) cachedPack(ctx context.Context, reader *repository.GitReader, shared *cache.Cache, wanted []string) (*cache.File, error) {
	if shared == nil || len(n.haves) != 0 {
		return nil, nil
	}
	ids := slices.Clone(wanted)
	slices.Sort(ids)
	reservation, err := reader.PackSizeHint(ids)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	for _, id := range ids {
		_, _ = io.WriteString(digest, id+"\n")
	}
	key := reader.CacheKey("upload-pack-v1:" + hex.EncodeToString(digest.Sum(nil)))
	file, err := shared.LoadFile(ctx, key, reservation, func(ctx context.Context, out io.Writer) error {
		return writeRawPack(ctx, reader, ids, out)
	})
	if errors.Is(err, cache.ErrCapacity) {
		return nil, nil
	}
	return file, err
}

func writeRawPack(ctx context.Context, reader *repository.GitReader, wanted []string, out io.Writer) error {
	if err := reader.Prefetch(ctx, wanted); err != nil {
		return err
	}
	_, err := gitpack.Write(ctx, out, wanted, reader.Get, repository.PackLimits())
	return err
}

// uploadResponse owns either a pinned shared raw pack or a private, fully framed
// response file. Shared packs are framed while being sent, avoiding another
// temporary file for every HTTP clone. Close releases the pin on every path.
type uploadResponse struct {
	body     io.ReadCloser
	prelude  []byte
	sideband bool
	remove   string
}

func (r *uploadResponse) write(ctx context.Context, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(r.prelude) != 0 {
		if err := writeSSH(out, r.prelude); err != nil {
			return err
		}
	}
	return copyPack(ctx, out, r.body, r.sideband)
}

func (r *uploadResponse) Close() error {
	err := r.body.Close()
	if r.remove != "" {
		// #nosec G703 -- prepareUpload obtains remove only from os.CreateTemp.
		err = errors.Join(err, os.Remove(r.remove))
	}
	return err
}

func copyPack(ctx context.Context, out io.Writer, body io.Reader, sideband bool) error {
	writer := out
	if sideband {
		writer = sidebandWriter{out}
	}
	if _, err := io.Copy(writer, packContextReader{ctx: ctx, reader: body}); err != nil {
		return err
	}
	if sideband {
		return writeSSH(out, []byte("0000"))
	}
	return nil
}

type packContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r packContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
