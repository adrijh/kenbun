package ingest

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"

	"go.opentelemetry.io/collector/pdata/pprofile"

	"github.com/adrijh/kenbun/internal/store"
)

// ReadJSONL converts a file-exporter capture (one OTLP JSON batch per line),
// gzipped if the name ends in .gz.
func ReadJSONL(path string) ([]store.Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // batches are ~160KB, can be much larger
	var u pprofile.JSONUnmarshaler
	var out []store.Sample
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		pd, err := u.UnmarshalProfiles(sc.Bytes())
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		out = append(out, Convert(pd)...)
	}
	return out, sc.Err()
}
