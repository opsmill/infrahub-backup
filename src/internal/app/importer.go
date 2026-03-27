package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/location"
	"github.com/PlakarKorp/kloset/objects"
)

// StreamingImporter implements the kloset importer.Importer interface.
// It produces a single Record from a streaming data source (e.g., exec stdout pipe).
type StreamingImporter struct {
	hostname string
	pathname string                        // virtual path for the record (e.g., "/neo4j-backup.tar")
	fi       objects.FileInfo              // file metadata (size can be 0 for streaming)
	dataFunc func() (io.ReadCloser, error) // lazy data factory — called on first Read
}

// NewStreamingImporter creates an importer that produces a single record from a data factory.
// pathname is the virtual path in the snapshot (e.g., "/neo4j-backup.tar").
// fi provides file metadata. dataFunc is called lazily to obtain the data stream.
func NewStreamingImporter(hostname, pathname string, fi objects.FileInfo, dataFunc func() (io.ReadCloser, error)) *StreamingImporter {
	return &StreamingImporter{
		hostname: hostname,
		pathname: pathname,
		fi:       fi,
		dataFunc: dataFunc,
	}
}

func (imp *StreamingImporter) Origin() string {
	return imp.hostname
}

func (imp *StreamingImporter) Type() string {
	return "infrahub"
}

func (imp *StreamingImporter) Root() string {
	return "/"
}

func (imp *StreamingImporter) Flags() location.Flags {
	return 0
}

func (imp *StreamingImporter) Ping(_ context.Context) error {
	return nil
}

func (imp *StreamingImporter) Close(_ context.Context) error {
	return nil
}

// Import sends a root directory record and a single file record to the records channel.
func (imp *StreamingImporter) Import(_ context.Context, records chan<- *connectors.Record, _ <-chan *connectors.Result) error {
	defer close(records)

	// Root directory entry
	now := time.Now()
	dirInfo := objects.NewFileInfo("/", 0, os.ModeDir|0755, now, 0, 0, 0, 0, 0)
	records <- connectors.NewRecord("/", "", dirInfo, nil, func() (io.ReadCloser, error) {
		return nil, nil
	})

	// The single file record with lazy data factory
	records <- connectors.NewRecord(imp.pathname, "", imp.fi, nil, imp.dataFunc)

	return nil
}

// NewMemoryImporter creates an importer for in-memory data (e.g., metadata JSON).
func NewMemoryImporter(hostname, pathname string, data []byte) *StreamingImporter {
	now := time.Now()
	fi := objects.NewFileInfo(pathname, int64(len(data)), 0644, now, 0, 0, 0, 0, 0)
	return NewStreamingImporter(hostname, pathname, fi, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
}
