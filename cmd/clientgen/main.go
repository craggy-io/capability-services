// Command clientgen writes a typed Go client for capabilities described
// in JSON files: package clientgen, for a person at a shell. A program
// that generates as part of its own build calls the package instead.
//
//	go run github.com/craggy-io/capability-services/cmd/clientgen \
//	    -package hostclient -out ./internal/hostclient \
//	    -include artifacts.content@1,workers.do@1 -by-namespace \
//	    artifacts.json workers.json
//
// Each file holds a list of descriptors, or an object with the list
// under "capabilities": the HTTP listing (curl host/v1/) or the reply a
// provider process describes itself with. "-" reads standard input.
//
// The files are written into the -out folder, which is made if it is
// missing. A file there that an earlier run generated and this run does
// not is removed; nothing else in the folder is touched.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/craggy-io/capability-services/clientgen"
	"github.com/craggy-io/capability-services/registry"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("clientgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pkg := fs.String("package", "", "name of the generated Go package (required)")
	out := fs.String("out", "", "folder to write the files into (required)")
	include := fs.String("include", "", "comma-separated refs to generate, name@version or a bare name; default every descriptor")
	byNamespace := fs.Bool("by-namespace", false, "write a file for each namespace instead of one file")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: clientgen -package NAME -out DIR [-include REFS] [-by-namespace] DESCRIPTORS.json...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pkg == "" || *out == "" || fs.NArg() == 0 {
		fs.Usage()
		return errors.New("clientgen: -package, -out and at least one descriptor file are required")
	}
	var descriptors []registry.Descriptor
	for _, name := range fs.Args() {
		var data []byte
		var err error
		if name == "-" {
			data, err = io.ReadAll(stdin)
		} else {
			data, err = os.ReadFile(name)
		}
		if err != nil {
			return err
		}
		descs, err := clientgen.ParseDescriptors(data)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		descriptors = append(descriptors, descs...)
	}
	opts := clientgen.Options{Package: *pkg}
	for _, ref := range strings.Split(*include, ",") {
		if ref = strings.TrimSpace(ref); ref != "" {
			opts.Include = append(opts.Include, ref)
		}
	}
	if *byNamespace {
		opts.Group = clientgen.ByNamespace
	}
	files, err := clientgen.Generate(descriptors, opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	// Remove what an earlier run generated and this one does not, so a
	// namespace that is no longer required leaves no file behind.
	entries, err := os.ReadDir(*out)
	if err != nil {
		return err
	}
	for _, e := range entries {
		path := filepath.Join(*out, e.Name())
		if _, keep := files[e.Name()]; keep || e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if old, err := os.ReadFile(path); err == nil && bytes.HasPrefix(old, []byte(clientgen.Header)) {
			if err := os.Remove(path); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "removed", path)
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(*out, name)
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "wrote", path)
	}
	return nil
}
