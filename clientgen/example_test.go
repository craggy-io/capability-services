package clientgen_test

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/craggy-io/capability-services/clientgen"
	"github.com/craggy-io/capability-services/registry"
)

type getIn struct {
	ID string `json:"id" jsonschema:"artifact id"`
}

type artifact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`
	Data []byte `json:"data,omitempty"`
}

type listIn struct {
	registry.PageIn
	Kind string `json:"kind,omitempty"`
}

// declarations prints the top-level declarations of generated source.
func declarations(src []byte) {
	for _, line := range strings.Split(string(src), "\n") {
		for _, keyword := range []string{"const ", "type ", "func "} {
			if strings.HasPrefix(line, keyword) && !strings.HasSuffix(line, "(") {
				fmt.Println(strings.TrimSuffix(line, " {"))
			}
		}
	}
}

// A typed client for a registry's capabilities: a ref constant, an input
// and an output type and a function for each, and for a capability that
// pages (cursor in, next out) the functions that read every page. The
// source is returned, never written: the caller decides where it goes.
func ExampleGenerate() {
	r := registry.New()
	defer r.Close()
	err := r.Add(
		registry.Func("storage.artifact.get", "Get an artifact's metadata",
			func(ctx context.Context, in getIn) (artifact, error) { return artifact{ID: in.ID}, nil }),
		registry.Func("storage.artifact.list", "List artifacts",
			func(ctx context.Context, in listIn) (registry.Page[artifact], error) {
				return registry.Paginate([]artifact{}, in.PageIn)
			}),
	)
	if err != nil {
		panic(err)
	}

	files, err := clientgen.Generate(r.Descriptors(false), clientgen.Options{Package: "storage"})
	if err != nil {
		panic(err)
	}
	for name, src := range files {
		fmt.Println(name, strings.HasPrefix(string(src), clientgen.Header))
		declarations(src)
	}
	// Output:
	// client.go true
	// type Caller interface
	// func call(ctx context.Context, c Caller, ref string, in, out any) error
	// const StorageArtifactGetRef = "storage.artifact.get@1"
	// type StorageArtifactGetIn struct
	// type StorageArtifactGetOut struct
	// func StorageArtifactGet(ctx context.Context, c Caller, in StorageArtifactGetIn) (StorageArtifactGetOut, error)
	// const StorageArtifactListRef = "storage.artifact.list@1"
	// type StorageArtifactListIn struct
	// type StorageArtifactListOut struct
	// type StorageArtifactListOutItemsItem struct
	// func StorageArtifactList(ctx context.Context, c Caller, in StorageArtifactListIn) (StorageArtifactListOut, error)
	// func StorageArtifactListPages(ctx context.Context, c Caller, in StorageArtifactListIn) iter.Seq2[StorageArtifactListOut, error]
	// func StorageArtifactListAll(ctx context.Context, c Caller, in StorageArtifactListIn) ([]StorageArtifactListOutItemsItem, error)
}

// Descriptors that arrive as JSON from another process (here, the reply
// a provider describes itself with), and a client for only the
// capabilities a caller requires, a file for each namespace.
func ExampleParseDescriptors() {
	reply := `{"type":"describe-reply","capabilities":[
		{"name":"blobs.read","version":1,"title":"Read a window of a blob",
		 "input":{"type":"object","properties":{"id":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["id"]},
		 "output":{"type":"object","properties":{"data":{"type":"string","contentEncoding":"base64"},"eof":{"type":"boolean"}},"required":["data","eof"]}},
		{"name":"blobs.delete","version":1,"input":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},
		{"name":"tools.run","version":2,"long":true,"input":{"type":"object","properties":{"tool":{"type":"string","enum":["scan","pack"]}},"required":["tool"]}}]}`
	descriptors, err := clientgen.ParseDescriptors([]byte(reply))
	if err != nil {
		panic(err)
	}
	files, err := clientgen.Generate(descriptors, clientgen.Options{
		Package: "host",
		Include: []string{"blobs.read@1", "tools.run@2"},
		Group:   clientgen.ByNamespace,
	})
	if err != nil {
		panic(err)
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Println(names)
	declarations(files["blobs_client.go"])
	declarations(files["tools_client.go"])

	// A name derived twice is an error, never a rename.
	_, err = clientgen.Generate([]registry.Descriptor{{Name: "blobs.read-all", Version: 1}, {Name: "blobs.read.all", Version: 1}},
		clientgen.Options{Package: "host"})
	fmt.Println(err)
	// Output:
	// [blobs_client.go client.go tools_client.go]
	// const BlobsReadRef = "blobs.read@1"
	// type BlobsReadIn struct
	// type BlobsReadOut struct
	// func BlobsRead(ctx context.Context, c Caller, in BlobsReadIn) (BlobsReadOut, error)
	// func BlobsReadPages(ctx context.Context, c Caller, in BlobsReadIn) iter.Seq2[BlobsReadOut, error]
	// func BlobsReadAll(ctx context.Context, c Caller, in BlobsReadIn) ([]byte, error)
	// const ToolsRunRef = "tools.run@2"
	// type ToolsRunIn struct
	// type ToolsRunInTool string
	// type ToolsRunOut = json.RawMessage
	// func ToolsRun(ctx context.Context, c Caller, in ToolsRunIn) (ToolsRunOut, error)
	// clientgen: blobs.read-all@1 and blobs.read.all@1 would both be named BlobsReadAll
}
