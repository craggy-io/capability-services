// This file is compiled and run beside the client generated from the
// describe replies of the Craggy subsystems artifacts and workers
// (testdata/describe), in a module that requires nothing (see
// TestGeneratedCodeBehaves). It is under testdata so that the go tool
// never builds it where it lies.
//
// It holds copies of the hand-written types and page-reading loops of
// the client package those subsystems' callers share today
// (marketplace-bundles lib/hostkit, as of 2026-11-13; the jsonschema tags
// are left out, they do not change the JSON), and checks the generated
// client against them: an input goes on the wire as the same bytes, an
// output is read to the same values, and reading every page asks for
// the same pages and returns the same result.
package hostclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"
)

// ---- copied from lib/hostkit ----

type FileEntry struct {
	Path     string `json:"path"`
	FilePath string `json:"filePath"`
	Size     int64  `json:"size"`
	Digest   string `json:"digest"`
}

type FilesOut struct {
	Form   string      `json:"form"`
	Name   string      `json:"name"`
	Digest string      `json:"digest"`
	Size   int64       `json:"size"`
	Files  []FileEntry `json:"files"`
}

type GetOut struct {
	Form         string `json:"form"`
	Digest       string `json:"digest"`
	ActivityCode string `json:"activityCode,omitempty"`
}

type RecordIn struct {
	Artifact string            `json:"artifact"`
	Code     string            `json:"code"`
	Version  string            `json:"version"`
	Params   json.RawMessage   `json:"params,omitempty"`
	Suffixes []string          `json:"suffixes"`
	Outputs  map[string][]byte `json:"outputs"`
	MaxBytes int64             `json:"maxBytes,omitempty"`
}

type RecordOut struct {
	Outputs map[string]string `json:"outputs"`
	Run     string            `json:"run"`
	Reused  bool              `json:"reused"`
	Data    map[string][]byte `json:"data,omitempty"`
}

type RunInput struct {
	Role string `json:"role"`
	ID   string `json:"id"`
}

type TableColumn struct {
	Name  string `json:"name"`
	Shape string `json:"shape"`
}

type RecordArtifactsIn struct {
	Capability   string           `json:"capability"`
	CodeVersion  string           `json:"codeVersion,omitempty"`
	NoInputs     bool             `json:"noInputs,omitempty"`
	SessionName  string           `json:"sessionName,omitempty"`
	Params       json.RawMessage  `json:"params,omitempty"`
	Inputs       []RunInput       `json:"inputs"`
	OutputName   string           `json:"outputName"`
	Table        bool             `json:"table,omitempty"`
	Blob         []byte           `json:"blob,omitempty"`
	TableSchema  string           `json:"tableSchema,omitempty"`
	TableKey     []string         `json:"tableKey,omitempty"`
	TableColumns []TableColumn    `json:"tableColumns,omitempty"`
	TableRows    []map[string]any `json:"tableRows,omitempty"`
}

type RecordArtifactsOut struct {
	ArtifactID string `json:"artifactId"`
	RunID      string `json:"runId"`
	Reused     bool   `json:"reused"`
	Data       []byte `json:"data,omitempty"`
	Rows       int    `json:"rows,omitempty"`
}

type RunIdentity struct {
	Capability  string          `json:"capability"`
	CodeKind    string          `json:"codeKind,omitempty"`
	CodeName    string          `json:"codeName,omitempty"`
	CodeVersion string          `json:"codeVersion,omitempty"`
	NoInputs    bool            `json:"noInputs,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	Inputs      []RunInput      `json:"inputs,omitempty"`
}

type RecordTreeIn struct {
	RunIdentity
	Deterministic bool              `json:"deterministic,omitempty"`
	SessionName   string            `json:"sessionName,omitempty"`
	OutputName    string            `json:"outputName"`
	Files         map[string][]byte `json:"files,omitempty"`
	Dir           string            `json:"dir,omitempty"`
	Blobs         map[string][]byte `json:"blobs,omitempty"`
	BlobPaths     map[string]string `json:"blobPaths,omitempty"`
}

type RecordTreeOut struct {
	ArtifactID string            `json:"artifactId"`
	RunID      string            `json:"runId"`
	Reused     bool              `json:"reused"`
	Outputs    map[string]string `json:"outputs"`
}

type FindRunIn struct {
	RunIdentity
	OutputName string   `json:"outputName"`
	Blobs      []string `json:"blobs,omitempty"`
}

type DoInput struct {
	Role         string `json:"role"`
	ID           string `json:"id"`
	Path         string `json:"path,omitempty"`
	As           string `json:"as"`
	Tree         bool   `json:"tree,omitempty"`
	SelectPrefix string `json:"selectPrefix,omitempty"`
	MaxBytes     int64  `json:"maxBytes,omitempty"`
}

type DoIn struct {
	Worker        string            `json:"worker"`
	Inputs        []DoInput         `json:"inputs,omitempty"`
	Files         map[string][]byte `json:"files,omitempty"`
	Params        json.RawMessage   `json:"params,omitempty"`
	Deterministic bool              `json:"deterministic,omitempty"`
	Output        string            `json:"output,omitempty"`
	CPUs          float64           `json:"cpus,omitempty"`
	MemoryMB      int64             `json:"memoryMb,omitempty"`
	DiskMB        int64             `json:"diskMb,omitempty"`
	TimeoutMS     int64             `json:"timeoutMs,omitempty"`
	Capture       []string          `json:"capture,omitempty"`
	Blobs         map[string]string `json:"blobs,omitempty"`
	PlanTree      map[string][]byte `json:"planTree,omitempty"`
}

type FileInfo struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type DoOut struct {
	Output   string            `json:"output"`
	Run      string            `json:"run"`
	Worker   string            `json:"worker"`
	Digest   string            `json:"digest"`
	Image    string            `json:"image"`
	EnvID    string            `json:"envId"`
	Elapsed  string            `json:"elapsed"`
	Reused   bool              `json:"reused"`
	Outputs  map[string]string `json:"outputs,omitempty"`
	Captured map[string][]byte `json:"captured,omitempty"`
	Primary  []FileInfo        `json:"primary,omitempty"`
}

type ExecIn struct {
	Worker string   `json:"worker"`
	Args   []string `json:"args,omitempty"`
}

type ExecOut struct {
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
	Exit   int    `json:"exit"`
}

// hostCall is servekit.Host.Call: one call through the host.
type hostCall func(ctx context.Context, ref string, in any) (json.RawMessage, error)

// callInto is servekit.Host.CallInto.
func callInto(ctx context.Context, call hostCall, ref string, in, out any) error {
	raw, err := call(ctx, ref, in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// readBlob is hostkit.ReadBlob.
func readBlob(ctx context.Context, call hostCall, artifact string, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 64 << 20
	}
	var out []byte
	for offset := int64(0); ; {
		chunk := min(limit-int64(len(out)), 65536)
		if chunk <= 0 {
			return out, nil
		}
		var page struct {
			Data []byte `json:"data"`
			EOF  bool   `json:"eof"`
		}
		err := callInto(ctx, call, "artifacts.content@1", map[string]any{"artifactId": artifact, "offset": offset, "limit": chunk}, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		offset += int64(len(page.Data))
		if page.EOF || len(page.Data) == 0 {
			return out, nil
		}
	}
}

const rangeBytes = 4 << 20

// fileRanges is the reader hostkit.OpenFile returns.
type fileRanges struct {
	ctx      context.Context
	call     hostCall
	artifact string
	path     string
	offset   int64
	page     []byte
	eof      bool
}

func (r *fileRanges) Read(into []byte) (int, error) {
	for len(r.page) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		raw, err := r.call(r.ctx, "artifacts.file.read@1", map[string]any{"artifact": r.artifact, "path": r.path, "offset": r.offset, "length": int64(rangeBytes)})
		if err != nil {
			return 0, err
		}
		var out struct {
			Data []byte `json:"data"`
			EOF  bool   `json:"eof"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return 0, err
		}
		r.page, r.eof = out.Data, out.EOF || len(out.Data) == 0
		r.offset += int64(len(out.Data))
	}
	n := copy(into, r.page)
	r.page = r.page[n:]
	return n, nil
}

const rowsPage = 1000

// readRows is hostkit.ReadRows.
func readRows(ctx context.Context, call hostCall, artifact string) ([]map[string]any, error) {
	var rows []map[string]any
	for {
		raw, err := call(ctx, "artifacts.rows@1", map[string]any{"artifactId": artifact, "offset": len(rows), "limit": rowsPage})
		if err != nil {
			return nil, err
		}
		var page struct {
			Rows []map[string]any `json:"rows"`
			EOF  bool             `json:"eof"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&page); err != nil {
			return nil, err
		}
		rows = append(rows, page.Rows...)
		if page.EOF || len(page.Rows) == 0 {
			return rows, nil
		}
	}
}

// ---- the checks ----

func marshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// sameInput checks that the generated input type G puts on the wire
// what hostkit's H does: for the value given, which sets every field,
// and for the zero value. The value crosses from H to G as JSON, and G
// must have a field for every member H writes. The bytes are the same
// unless differs names how the provider's own schema differs from
// hostkit's copy of it: "order" when it lists the same members in
// another order, and any other word is a member hostkit's zero value
// sends as null where the provider's schema makes it optional, so the
// generated type leaves it out.
func sameInput[H, G any](t *testing.T, name string, full H, differs ...string) {
	t.Helper()
	for what, h := range map[string]H{"full": full, "zero": *new(H)} {
		sent := marshal(t, h)
		var g G
		dec := json.NewDecoder(bytes.NewReader([]byte(sent)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&g); err != nil {
			t.Errorf("%s (%s): the generated type cannot hold what hostkit sends: %v", name, what, err)
			continue
		}
		got := marshal(t, g)
		if len(differs) == 0 {
			if got != sent {
				t.Errorf("%s (%s):\ngenerated %s\nhostkit   %s", name, what, got, sent)
			}
			continue
		}
		var want, have map[string]any
		if err := json.Unmarshal([]byte(sent), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(got), &have); err != nil {
			t.Fatal(err)
		}
		for _, member := range differs {
			if v, ok := want[member]; ok && v == nil {
				delete(want, member)
			}
		}
		if !reflect.DeepEqual(have, want) {
			t.Errorf("%s (%s) differs in more than the order of its members:\ngenerated %s\nhostkit   %s", name, what, got, sent)
		}
	}
}

// sameOutput checks that an output read through the generated type G
// holds what hostkit's H reads from the same JSON: doc is read into G,
// and H read from G's JSON equals H read from doc.
func sameOutput[H, G any](t *testing.T, name, doc string) {
	t.Helper()
	var direct, through H
	var g G
	if err := json.Unmarshal([]byte(doc), &direct); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if err := json.Unmarshal([]byte(doc), &g); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if err := json.Unmarshal([]byte(marshal(t, g)), &through); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !reflect.DeepEqual(direct, through) {
		t.Errorf("%s:\nthrough the generated type %+v\nhostkit                    %+v", name, through, direct)
	}
	if reflect.DeepEqual(direct, *new(H)) {
		t.Errorf("%s: the document set nothing hostkit reads", name)
	}
}

func TestInputsGoOnTheWireAsHostkitSendsThem(t *testing.T) {
	params := json.RawMessage(`{"depth":3,"mode":"fast"}`)
	inputs := []RunInput{{Role: "artifact", ID: "art-1"}, {Role: "rules", ID: ""}}
	identity := RunIdentity{Capability: "binary.strings", CodeKind: "worker", CodeName: "craggy.strings", CodeVersion: "2",
		NoInputs: true, Params: params, Inputs: inputs}

	sameInput[RecordIn, ArtifactsRunsRecordIn](t, "RecordIn", RecordIn{
		Artifact: "art-1", Code: "secrets.scan", Version: "3", Params: params, Suffixes: []string{".report.json"},
		Outputs: map[string][]byte{".report.json": []byte("{}"), ".raw": {0, 255}}, MaxBytes: 1 << 20})
	sameInput[RecordArtifactsIn, ArtifactsRunsRecordArtifactsIn](t, "RecordArtifactsIn", RecordArtifactsIn{
		Capability: "ingest.table", CodeVersion: "2", NoInputs: true, SessionName: "s", Params: params, Inputs: inputs,
		OutputName: "out", Table: true, Blob: []byte("blob"), TableSchema: "rows@1", TableKey: []string{"id"},
		TableColumns: []TableColumn{{Name: "id", Shape: "int"}},
		TableRows:    []map[string]any{{"id": 1.0, "name": "a", "tags": []any{"x"}}}},
		// The provider lists sessionName after inputs, and inputs is
		// optional there: hostkit's zero value sends "inputs": null.
		"order", "inputs")
	sameInput[RecordTreeIn, ArtifactsRunsRecordTreeIn](t, "RecordTreeIn", RecordTreeIn{
		RunIdentity: identity, Deterministic: true, SessionName: "s", OutputName: ".out",
		Files: map[string][]byte{"a/b.txt": []byte("hi")}, Dir: "/tmp/out",
		Blobs: map[string][]byte{".log": []byte("log")}, BlobPaths: map[string]string{".big": "/tmp/big"}})
	sameInput[FindRunIn, ArtifactsRunsFindIn](t, "FindRunIn", FindRunIn{
		RunIdentity: identity, OutputName: ".out", Blobs: []string{".log"}})
	sameInput[DoIn, WorkersDoIn](t, "DoIn", DoIn{
		Worker: "craggy.afl",
		Inputs: []DoInput{{Role: "artifact", ID: "art-1", Path: "bin/app", As: "target", Tree: true, SelectPrefix: "bin/", MaxBytes: 9}},
		Files:  map[string][]byte{"seed": {1, 2, 3}}, Params: params, Deterministic: true, Output: ".fuzz",
		CPUs: 1.5, MemoryMB: 2048, DiskMB: 4096, TimeoutMS: 60000, Capture: []string{"report.json"},
		Blobs: map[string]string{"report.json": ".report.json"}, PlanTree: map[string][]byte{"plan.yaml": []byte("a: 1")}})
	sameInput[ExecIn, WorkersExecIn](t, "ExecIn", ExecIn{Worker: "craggy.adb", Args: []string{"devices", "-l"}})

	// The inputs hostkit builds as maps, against the generated structs.
	for name, pair := range map[string][2]any{
		"artifacts.content":           {map[string]any{"artifactId": "art-1", "offset": int64(65536), "limit": int64(65536)}, ArtifactsContentIn{ArtifactId: "art-1", Offset: 65536, Limit: 65536}},
		"artifacts.files":             {map[string]any{"artifact": "art-1"}, ArtifactsFilesIn{Artifact: "art-1"}},
		"artifacts.get":               {map[string]any{"artifact": "art-1"}, ArtifactsGetIn{Artifact: "art-1"}},
		"artifacts.file.read (whole)": {map[string]any{"artifact": "art-1", "path": "a/b", "limit": int64(100)}, ArtifactsFileReadIn{Artifact: "art-1", Path: "a/b", Limit: 100}},
		"artifacts.file.read (range)": {map[string]any{"artifact": "art-1", "path": "a/b", "offset": int64(8), "length": int64(rangeBytes)}, ArtifactsFileReadIn{Artifact: "art-1", Path: "a/b", Offset: 8, Length: rangeBytes}},
		"artifacts.rows":              {map[string]any{"artifactId": "t-1", "offset": 1000, "limit": rowsPage}, ArtifactsRowsIn{ArtifactId: "t-1", Offset: 1000, Limit: rowsPage}},
		"workers.ready":               {map[string]any{"worker": "craggy.afl"}, WorkersReadyIn{Worker: "craggy.afl"}},
	} {
		var want, got any
		if err := json.Unmarshal([]byte(marshal(t, pair[0])), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(marshal(t, pair[1])), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: generated %s, hostkit %s", name, marshal(t, pair[1]), marshal(t, pair[0]))
		}
	}
}

func TestOutputsAreReadAsHostkitReadsThem(t *testing.T) {
	sameOutput[FilesOut, ArtifactsFilesOut](t, "FilesOut",
		`{"form":"tree","name":"app","digest":"sha256:aa","size":12,"files":[{"path":"a/b","filePath":"a/b","size":12,"digest":"sha256:bb"}]}`)
	sameOutput[GetOut, ArtifactsGetOut](t, "GetOut", `{"form":"blob","digest":"sha256:aa","activityCode":"binary.strings"}`)
	sameOutput[RecordOut, ArtifactsRunsRecordOut](t, "RecordOut",
		`{"outputs":{".report.json":"art-2"},"run":"run-1","reused":true,"data":{".report.json":"e30="}}`)
	sameOutput[RecordArtifactsOut, ArtifactsRunsRecordArtifactsOut](t, "RecordArtifactsOut",
		`{"artifactId":"art-2","runId":"run-1","reused":true,"data":"YmxvYg==","rows":3}`)
	sameOutput[RecordTreeOut, ArtifactsRunsRecordTreeOut](t, "RecordTreeOut",
		`{"artifactId":"art-2","runId":"run-1","reused":true,"outputs":{".out":"art-2",".log":"art-3"}}`)
	// FindRun reads found beside the RecordTreeOut members.
	sameOutput[struct {
		Found bool `json:"found"`
		RecordTreeOut
	}, ArtifactsRunsFindOut](t, "FindRun", `{"found":true,"artifactId":"art-2","runId":"run-1","reused":true,"outputs":{".out":"art-2"}}`)
	sameOutput[DoOut, WorkersDoOut](t, "DoOut",
		`{"output":"art-9","run":"run-2","worker":"craggy.afl","digest":"sha256:cc","image":"sha256:dd","envId":"env-1","elapsed":"1.5s","reused":true,`+
			`"outputs":{".fuzz":"art-9"},"captured":{"report.json":"e30="},"primary":[{"path":"bin/app","size":9,"digest":"sha256:ee"}]}`)
	sameOutput[ExecOut, WorkersExecOut](t, "ExecOut", `{"stdout":"b3V0","stderr":"ZXJy","exit":3}`)
	sameOutput[struct {
		Ready bool `json:"ready"`
	}, WorkersReadyOut](t, "WorkerReady", `{"ready":true}`)
	// ReadBlobHead and ReadFile read these members of the two read outputs.
	sameOutput[struct {
		Data   []byte `json:"data"`
		Digest string `json:"digest"`
	}, ArtifactsContentOut](t, "ReadBlobHead", `{"artifactId":"a","digest":"sha256:aa","form":"blob","offset":0,"total":4,"data":"AAEC+g==","eof":true}`)
	sameOutput[struct {
		Data   []byte `json:"data"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	}, ArtifactsFileReadOut](t, "ReadFile", `{"data":"AAEC+g==","digest":"sha256:aa","size":4,"eof":true}`)
}

// store is a fake host: it serves artifacts.content, artifacts.file.read
// and artifacts.rows over what it holds, a page of at most its page
// size, and records every request as JSON. It takes hostkit's map
// inputs and the generated struct inputs alike, as a real host does: as
// JSON.
type store struct {
	blob     []byte
	rows     []string // each one row, as JSON
	page     int      // the most bytes or rows one call returns
	requests []string
}

func (s *store) Call(_ context.Context, ref string, in any) (json.RawMessage, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var req struct {
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
		Length int `json:"length"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Requests are compared as values: a map and a struct order their
	// members differently, and hostkit sends "offset": 0 where a
	// generated struct leaves an optional zero out. The capability
	// reads both as 0.
	var canonical map[string]any
	_ = json.Unmarshal(raw, &canonical)
	canonical["offset"] = req.Offset
	again, _ := json.Marshal(canonical)
	s.requests = append(s.requests, ref+" "+string(again))
	switch ref {
	case "artifacts.content@1", "artifacts.file.read@1":
		n := min(s.page, req.Limit)
		if ref == "artifacts.file.read@1" {
			n = min(s.page, req.Length)
		}
		start := min(req.Offset, len(s.blob))
		end := min(start+n, len(s.blob))
		return json.Marshal(map[string]any{"data": s.blob[start:end], "eof": end == len(s.blob), "digest": "sha256:aa", "size": len(s.blob), "total": len(s.blob)})
	case "artifacts.rows@1":
		start := min(req.Offset, len(s.rows))
		end := min(start+min(s.page, req.Limit), len(s.rows))
		page := `{"rows":[`
		for i, row := range s.rows[start:end] {
			if i > 0 {
				page += ","
			}
			page += row
		}
		eof := "false"
		if end == len(s.rows) {
			eof = "true"
		}
		return json.RawMessage(page + `],"eof":` + eof + `}`), nil
	}
	return nil, io.ErrUnexpectedEOF
}

func pattern(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*31 + i>>8)
	}
	return data
}

// ArtifactsContentAll does what hostkit.ReadBlob does: the same calls,
// the same bytes.
func TestReadAllOverContentIsReadBlob(t *testing.T) {
	ctx := context.Background()
	for _, size := range []int{0, 1, 65535, 65536, 65537, 200_000} {
		for _, page := range []int{65536, 1000} { // a host that returns what is asked, and one that returns less
			if size > 70_000 && page == 1000 {
				continue
			}
			data := pattern(size)
			hk, gen := &store{blob: data, page: page}, &store{blob: data, page: page}
			want, err := readBlob(ctx, hk.Call, "art-1", 0)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ArtifactsContentAll(ctx, gen, ArtifactsContentIn{ArtifactId: "art-1", Limit: 65536})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) || !bytes.Equal(got, data) {
				t.Errorf("size %d, page %d: generated read %d bytes, hostkit %d", size, page, len(got), len(want))
			}
			if !reflect.DeepEqual(gen.requests, hk.requests) {
				t.Errorf("size %d, page %d: generated asked %v, hostkit %v", size, page, gen.requests, hk.requests)
			}
		}
	}
}

// ReadBlob also takes a limit on the whole read. That is the caller's
// to write over the pages: this is all of it.
func TestBoundedReadOverContentPages(t *testing.T) {
	ctx := context.Background()
	data := pattern(200_000)
	const limit = 100_000
	hk, gen := &store{blob: data, page: 65536}, &store{blob: data, page: 65536}
	want, err := readBlob(ctx, hk.Call, "art-1", limit)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for page, err := range ArtifactsContentPages(ctx, gen, ArtifactsContentIn{ArtifactId: "art-1", Limit: 65536}) {
		if err != nil {
			t.Fatal(err)
		}
		if got = append(got, page.Data...); len(got) >= limit {
			got = got[:limit]
			break
		}
	}
	if !bytes.Equal(got, want) || len(got) != limit {
		t.Errorf("bounded read: generated %d bytes, hostkit %d", len(got), len(want))
	}
}

// ArtifactsFileReadAll with a range length reads what hostkit.OpenFile
// streams, with the same calls. (OpenFile is an io.Reader that holds
// one range at a time; All holds the file.)
func TestReadAllOverFileRangesIsOpenFile(t *testing.T) {
	ctx := context.Background()
	data := pattern(10_000)
	hk, gen := &store{blob: data, page: 4096}, &store{blob: data, page: 4096}
	want, err := io.ReadAll(&fileRanges{ctx: ctx, call: hk.Call, artifact: "tree-1", path: "a/b.bin"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ArtifactsFileReadAll(ctx, gen, ArtifactsFileReadIn{Artifact: "tree-1", Path: "a/b.bin", Length: rangeBytes})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || !bytes.Equal(got, data) {
		t.Errorf("generated read %d bytes, hostkit %d", len(got), len(want))
	}
	if !reflect.DeepEqual(gen.requests, hk.requests) {
		t.Errorf("generated asked %v, hostkit %v", gen.requests, hk.requests)
	}
}

// ArtifactsRowsAll does what hostkit.ReadRows does, down to an integer
// too large for a float64 arriving as it was stored.
func TestReadAllOverRowsIsReadRows(t *testing.T) {
	ctx := context.Background()
	rows := []string{`{"id":9007199254740993,"score":9.5}`, `{"id":2,"score":null}`, `{"id":3,"score":1,"tags":["a"]}`}
	hk, gen := &store{rows: rows, page: 2}, &store{rows: rows, page: 2}
	want, err := readRows(ctx, hk.Call, "table-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ArtifactsRowsAll(ctx, gen, ArtifactsRowsIn{ArtifactId: "table-1", Limit: rowsPage})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || len(got) != 3 {
		t.Errorf("generated %v, hostkit %v", got, want)
	}
	if id, ok := got[0]["id"].(json.Number); !ok || id.String() != "9007199254740993" {
		t.Errorf("first id = %#v, want the integer as stored", got[0]["id"])
	}
	if !reflect.DeepEqual(gen.requests, hk.requests) {
		t.Errorf("generated asked %v, hostkit %v", gen.requests, hk.requests)
	}
}

// The typed functions make the calls hostkit's make.
func TestTypedCalls(t *testing.T) {
	ctx := context.Background()
	var ref, sent string
	caller := callerFunc(func(_ context.Context, r string, in any) (json.RawMessage, error) {
		raw, err := json.Marshal(in)
		ref, sent = r, string(raw)
		return json.RawMessage(`{"output":"art-9","run":"run-2","worker":"craggy.afl","digest":"d","image":"i","envId":"e","elapsed":"1s","reused":false,"captured":{"report.json":"e30="}}`), err
	})
	out, err := WorkersDo(ctx, caller, WorkersDoIn{Worker: "craggy.afl", Capture: []string{"report.json"}, Blobs: map[string]string{"report.json": ".report.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "workers.do@1" || sent != `{"worker":"craggy.afl","capture":["report.json"],"blobs":{"report.json":".report.json"}}` {
		t.Errorf("called %s with %s", ref, sent)
	}
	if out.Output != "art-9" || string(out.Captured["report.json"]) != "{}" {
		t.Errorf("output = %+v", out)
	}
}

type callerFunc func(ctx context.Context, ref string, in any) (json.RawMessage, error)

func (f callerFunc) Call(ctx context.Context, ref string, in any) (json.RawMessage, error) {
	return f(ctx, ref, in)
}
