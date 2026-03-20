package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/snapshot/importer"
)

// StreamingImporter implements the kloset importer.Importer interface.
// It produces a single Record from a streaming data source (e.g., exec stdout pipe).
type StreamingImporter struct {
	hostname string
	pathname string                      // virtual path for the record (e.g., "/neo4j-backup.tar")
	fi       objects.FileInfo            // file metadata (size can be 0 for streaming)
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

func (imp *StreamingImporter) Origin(_ context.Context) (string, error) {
	return imp.hostname, nil
}

func (imp *StreamingImporter) Type(_ context.Context) (string, error) {
	return "infrahub", nil
}

func (imp *StreamingImporter) Root(_ context.Context) (string, error) {
	return "/", nil
}

func (imp *StreamingImporter) Close(_ context.Context) error {
	return nil
}

// Scan returns a channel with a root directory record and a single file record.
func (imp *StreamingImporter) Scan(_ context.Context) (<-chan *importer.ScanResult, error) {
	ch := make(chan *importer.ScanResult, 2)

	go func() {
		defer close(ch)

		// Root directory entry
		now := time.Now()
		dirInfo := objects.NewFileInfo("/", 0, os.ModeDir|0755, now, 0, 0, 0, 0, 0)
		ch <- importer.NewScanRecord("/", "", dirInfo, nil, func() (io.ReadCloser, error) {
			return io.NopCloser(&emptyReader{}), nil
		})

		// The single file record with lazy data factory
		ch <- importer.NewScanRecord(imp.pathname, "", imp.fi, nil, imp.dataFunc)
	}()

	return ch, nil
}

// NewMemoryImporter creates an importer for in-memory data (e.g., metadata JSON).
func NewMemoryImporter(hostname, pathname string, data []byte) *StreamingImporter {
	now := time.Now()
	fi := objects.NewFileInfo(pathname, int64(len(data)), 0644, now, 0, 0, 0, 0, 0)
	return NewStreamingImporter(hostname, pathname, fi, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
