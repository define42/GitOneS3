package gittransport

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/define42/GitOneS3/internal/repository"
)

// The branches share both history and an unchanged blob. Their merge and nested
// annotated tags exercise graph subtraction beyond a linear commit chain.
func uploadFixture(t testing.TB) (*repository.GitSnapshot, map[string]string) {
	t.Helper()
	snapshot := &repository.GitSnapshot{
		DefaultBranch: "main", References: map[string]string{}, Objects: map[string]repository.GitObject{},
	}
	ids := map[string]string{}
	add := func(name, kind, data string) string {
		object := repository.GitObject{Type: kind, Data: []byte(data)}
		id := repository.GitObjectID(object)
		snapshot.Objects[id], ids[name] = object, id
		return id
	}
	tree := func(name string, entries ...string) string {
		var data strings.Builder
		for _, entry := range entries {
			id, err := hex.DecodeString(ids[entry])
			if err != nil {
				t.Fatal(err)
			}
			data.WriteString("100644 " + entry + "\x00")
			data.Write(id)
		}
		return add(name, "tree", data.String())
	}
	commit := func(name, treeID string, parents ...string) string {
		data := "tree " + treeID + "\n"
		for _, parent := range parents {
			data += "parent " + parent + "\n"
		}
		data += "author Alice <alice@example.test> 1 +0000\ncommitter Alice <alice@example.test> 1 +0000\n\n" + name + "\n"
		return add(name, "commit", data)
	}
	add("stable", "blob", "unchanged\n")
	add("base-file", "blob", "base\n")
	base := commit("base", tree("base-tree", "base-file", "stable"))
	add("left-file", "blob", "left\n")
	left := commit("left", tree("left-tree", "left-file", "stable"), base)
	add("right-file", "blob", "right\n")
	right := commit("right", tree("right-tree", "right-file", "stable"), base)
	merge := commit("merge", tree("merge-tree", "left-file", "right-file", "stable"), left, right)
	tag := add("tag", "tag", "object "+left+"\ntype commit\ntag v1\ntagger Alice <alice@example.test> 1 +0000\n\nv1\n")
	nested := add("nested-tag", "tag", "object "+tag+"\ntype tag\ntag v2\ntagger Alice <alice@example.test> 1 +0000\n\nv2\n")
	snapshot.References = map[string]string{
		"refs/heads/main": merge, "refs/heads/left": left, "refs/heads/right": right,
		"refs/tags/v1": tag, "refs/tags/v2": nested,
		"refs/tags/blob": ids["stable"], "refs/tags/tree": ids["left-tree"],
	}
	return snapshot, ids
}

func uploadRequest(wants, haves []string, sideband bool, done bool) []byte {
	var request strings.Builder
	for i, id := range wants {
		capabilities := ""
		if i == 0 && sideband {
			capabilities = " side-band-64k"
		}
		request.WriteString(pkt("want " + id + capabilities + "\n"))
	}
	request.WriteString("0000")
	for _, id := range haves {
		request.WriteString(pkt("have " + id + "\n"))
	}
	if done {
		request.WriteString(pkt("done\n"))
	} else {
		request.WriteString("0000")
	}
	return []byte(request.String())
}

func uploadPackObjects(t *testing.T, response []byte, sideband bool) map[string]repository.GitObject {
	t.Helper()
	reader := bytes.NewReader(response)
	line, flush, err := readPkt(reader)
	if err != nil || flush || (string(line) != "NAK\n" && !strings.HasPrefix(string(line), "ACK ")) {
		t.Fatalf("invalid upload acknowledgement: %q, flush=%v, err=%v", line, flush, err)
	}
	pack := response[len(response)-reader.Len():]
	if sideband {
		var data bytes.Buffer
		for {
			line, flush, err := readPkt(reader)
			if err != nil {
				t.Fatal(err)
			}
			if flush {
				break
			}
			if len(line) == 0 || line[0] != 1 {
				t.Fatalf("invalid pack sideband: %q", line)
			}
			data.Write(line[1:])
		}
		if reader.Len() != 0 {
			t.Fatal("data after pack flush")
		}
		pack = data.Bytes()
	}
	objects, err := decodePack(t.Context(), pack, nil)
	if err != nil {
		t.Fatal(err)
	}
	return objects
}

func TestUploadIncrementalObjects(t *testing.T) {
	t.Parallel()
	snapshot, ids := uploadFixture(t)
	for _, tc := range []struct {
		name          string
		wants, haves  []string
		expectedNames []string
	}{
		{name: "clone", wants: []string{"left"}, expectedNames: []string{"base", "base-tree", "base-file", "stable", "left", "left-tree", "left-file"}},
		{name: "one commit", wants: []string{"left"}, haves: []string{"base"}, expectedNames: []string{"left", "left-tree", "left-file"}},
		{name: "already present", wants: []string{"left"}, haves: []string{"left"}},
		{name: "duplicate roots", wants: []string{"left", "left"}, haves: []string{"base", "base"}, expectedNames: []string{"left", "left-tree", "left-file"}},
		{name: "merge one parent", wants: []string{"merge"}, haves: []string{"left"}, expectedNames: []string{"right", "right-tree", "right-file", "merge", "merge-tree"}},
		{name: "merge both parents", wants: []string{"merge"}, haves: []string{"left", "right"}, expectedNames: []string{"merge", "merge-tree"}},
		{name: "multiple wants", wants: []string{"left", "right"}, haves: []string{"base"}, expectedNames: []string{"left", "left-tree", "left-file", "right", "right-tree", "right-file"}},
		{name: "other branch shares history", wants: []string{"left"}, haves: []string{"right"}, expectedNames: []string{"left", "left-tree", "left-file"}},
		{name: "nested annotated tags", wants: []string{"nested-tag"}, haves: []string{"left"}, expectedNames: []string{"tag", "nested-tag"}},
		{name: "tree tag", wants: []string{"left-tree"}, haves: []string{"base"}, expectedNames: []string{"left-tree", "left-file"}},
		{name: "blob tag already present", wants: []string{"stable"}, haves: []string{"base"}},
		{name: "unknown have", wants: []string{"left"}, haves: []string{"unknown"}, expectedNames: []string{"base", "base-tree", "base-file", "stable", "left", "left-tree", "left-file"}},
	} {
		for _, sideband := range []bool{false, true} {
			name := tc.name
			if sideband {
				name += "/sideband"
			}
			t.Run(name, func(t *testing.T) {
				resolve := func(names []string) []string {
					result := make([]string, 0, len(names))
					for _, name := range names {
						if name == "unknown" {
							result = append(result, strings.Repeat("f", 40))
						} else {
							result = append(result, ids[name])
						}
					}
					return result
				}
				response, err := (&Handler{}).upload(t.Context(), snapshot, uploadRequest(resolve(tc.wants), resolve(tc.haves), sideband, true))
				if err != nil {
					t.Fatal(err)
				}
				objects := uploadPackObjects(t, response, sideband)
				got, want := slices.Sorted(maps.Keys(objects)), resolve(tc.expectedNames)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Fatalf("pack objects = %v; want %v", got, want)
				}
			})
		}
	}
}

func TestUploadValidatesGraphsBeforeSubtraction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, have, corrupt string
	}{
		{name: "shared history", have: "base", corrupt: "base-file"},
		{name: "entire want omitted", have: "left", corrupt: "left-file"},
		{name: "have outside wanted history", have: "right", corrupt: "right-file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, ids := uploadFixture(t)
			delete(snapshot.Objects, ids[tc.corrupt])
			response, err := (&Handler{}).upload(t.Context(), snapshot, uploadRequest(
				[]string{ids["left"]}, []string{ids[tc.have]}, false, true,
			))
			if !errors.Is(err, repository.ErrInvalid) || len(response) != 0 {
				t.Fatalf("corrupt graph produced %d response bytes, err=%v", len(response), err)
			}
		})
	}
}

func TestUploadHTTPStatelessAcknowledgement(t *testing.T) {
	t.Parallel()
	snapshot, ids := uploadFixture(t)
	handler := &Handler{}
	for _, tc := range []struct {
		name  string
		haves []string
		want  string
	}{
		{name: "no common", haves: []string{strings.Repeat("f", 40)}, want: "NAK\n"},
		{name: "first common only", haves: []string{ids["base"], ids["left"]}, want: "ACK " + ids["base"] + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each POST reconstructs its own common set; ACK state cannot leak
			// from a previous response into this independent HTTP request.
			for range 2 {
				response, err := handler.upload(t.Context(), snapshot, uploadRequest(
					[]string{ids["left"]}, tc.haves, false, false,
				))
				if err != nil || string(response) != pkt(tc.want) {
					t.Fatalf("negotiation-only response = %q, %v; want %q", response, err, pkt(tc.want))
				}
			}
		})
	}
}

func TestUploadNegotiationValidation(t *testing.T) {
	t.Parallel()
	handler, snapshot := sshTestHandler(t)
	id := snapshot.References["refs/heads/main"]
	want := pkt("want " + id + "\n")
	have := pkt("have " + id + "\n")
	for _, tc := range []struct {
		name, body string
		limit      bool
	}{
		{name: "unadvertised want", body: pkt("want "+strings.Repeat("f", 40)+"\n") + "0000" + pkt("done\n")},
		{name: "shallow unsupported", body: pkt("want "+id+" shallow\n") + "0000" + pkt("done\n")},
		{name: "filter unsupported", body: pkt("want "+id+" filter\n") + "0000" + pkt("done\n")},
		{name: "multi ack unsupported", body: pkt("want "+id+" multi_ack_detailed\n") + "0000" + pkt("done\n")},
		{name: "have before wants", body: have + "0000" + pkt("done\n")},
		{name: "want after wants flush", body: want + "0000" + want + pkt("done\n")},
		{name: "invalid have id", body: want + "0000" + pkt("have not-an-id\n") + pkt("done\n")},
		{name: "extra have field", body: want + "0000" + pkt("have "+id+" extra\n") + pkt("done\n")},
		{name: "extra done field", body: want + "0000" + pkt("done extra\n")},
		{name: "oversize packet", body: "ffff"},
		{name: "packet count", body: want + "0000" + strings.Repeat(have, maxNegotiationPackets) + pkt("done\n"), limit: true},
		{name: "flush count", body: want + strings.Repeat("0000", maxNegotiationPackets) + pkt("done\n"), limit: true},
		{name: "byte count", body: strings.Repeat(pkt("want "+id+" agent="+strings.Repeat("a", 60000)+"\n"), 18) + "0000" + pkt("done\n"), limit: true},
	} {
		for _, ssh := range []bool{false, true} {
			name := tc.name + "/http"
			if ssh {
				name = tc.name + "/ssh"
			}
			t.Run(name, func(t *testing.T) {
				var err error
				if ssh {
					stream := &sshTestStream{input: bytes.NewReader([]byte(tc.body))}
					err = handler.uploadSSH(t.Context(), snapshot, stream)
				} else {
					_, err = handler.upload(t.Context(), snapshot, []byte(tc.body))
				}
				wantErr := errPack
				if tc.limit {
					wantErr = repository.ErrLimit
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("error = %v; want %v", err, wantErr)
				}
			})
		}
	}
}

func TestUploadNegotiationAtPacketLimit(t *testing.T) {
	t.Parallel()
	handler, snapshot := sshTestHandler(t)
	id := snapshot.References["refs/heads/main"]
	body := []byte(pkt("want "+id+"\n") + "0000" +
		strings.Repeat(pkt("have "+id+"\n"), maxNegotiationPackets-3) + pkt("done\n"))
	for _, ssh := range []bool{false, true} {
		var response []byte
		var err error
		if ssh {
			stream := &sshTestStream{input: bytes.NewReader(body)}
			err = handler.uploadSSH(t.Context(), snapshot, stream)
			response = stream.output.Bytes()
		} else {
			response, err = handler.upload(t.Context(), snapshot, body)
		}
		if err != nil {
			t.Fatalf("ssh=%v: %v", ssh, err)
		}
		if objects := uploadPackObjects(t, response, false); len(objects) != 0 {
			t.Fatalf("ssh=%v: expected empty incremental pack, got %d objects", ssh, len(objects))
		}
	}
}

func TestUploadNegotiationCancellation(t *testing.T) {
	t.Parallel()
	handler, snapshot := sshTestHandler(t)
	id := snapshot.References["refs/heads/main"]
	body := uploadRequest([]string{id}, nil, false, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := handler.upload(ctx, snapshot, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled HTTP negotiation: %v", err)
	}
	stream := &sshTestStream{input: bytes.NewReader(body)}
	if err := handler.uploadSSH(ctx, snapshot, stream); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled SSH negotiation: %v", err)
	}
	n := newUploadNegotiation(snapshot)
	if err := n.want([]byte("want " + id + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := n.pack(ctx, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled graph selection: %v", err)
	}
}
