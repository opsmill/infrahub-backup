package app

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/snapshot/importer"
	"github.com/sirupsen/logrus"
)

// InfrahubImporter implements the kloset importer.Importer interface.
// It produces Records from Infrahub database dumps stored in a temp directory.
type InfrahubImporter struct {
	hostname string
	tempDir  string // directory containing the dump files
}

// NewInfrahubImporter creates a new importer from a directory of dump files.
func NewInfrahubImporter(hostname, tempDir string) *InfrahubImporter {
	return &InfrahubImporter{
		hostname: hostname,
		tempDir:  tempDir,
	}
}

func (imp *InfrahubImporter) Origin(_ context.Context) (string, error) {
	return imp.hostname, nil
}

func (imp *InfrahubImporter) Type(_ context.Context) (string, error) {
	return "infrahub", nil
}

func (imp *InfrahubImporter) Root(_ context.Context) (string, error) {
	return "/", nil
}

func (imp *InfrahubImporter) Close(_ context.Context) error {
	return nil
}

// Scan walks the temp directory and sends each file as a ScanResult.
func (imp *InfrahubImporter) Scan(_ context.Context) (<-chan *importer.ScanResult, error) {
	ch := make(chan *importer.ScanResult, 32)

	go func() {
		defer close(ch)

		err := filepath.Walk(imp.tempDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				ch <- importer.NewScanError(path, err)
				return nil
			}

			// Compute pathname relative to tempDir, prefixed with /
			rel, err := filepath.Rel(imp.tempDir, path)
			if err != nil {
				ch <- importer.NewScanError(path, err)
				return nil
			}
			pathname := "/" + filepath.ToSlash(rel)
			if pathname == "/." {
				pathname = "/"
			}

			fi := objects.FileInfoFromStat(info)

			if info.IsDir() {
				ch <- importer.NewScanRecord(pathname, "", fi, nil, func() (io.ReadCloser, error) {
					return io.NopCloser(&emptyReader{}), nil
				})
				return nil
			}

			fullPath := path // capture for closure
			ch <- importer.NewScanRecord(pathname, "", fi, nil, func() (io.ReadCloser, error) {
				return os.Open(fullPath)
			})

			logrus.Debugf("Importer: queued %s (%d bytes)", pathname, info.Size())
			return nil
		})
		if err != nil {
			logrus.Warnf("Importer walk error: %v", err)
		}
	}()

	return ch, nil
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
