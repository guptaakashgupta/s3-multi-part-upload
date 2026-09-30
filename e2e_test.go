package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// Test_e2e_multipartVersions uploads two versions of one key through the real
// handlers and client, then checks both versions download byte-for-byte.
func Test_e2e_multipartVersions(t *testing.T) {
	fakeS3 := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	defer fakeS3.Close()
	t.Setenv("S3_ENDPOINT", fakeS3.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	fs, err := newFileService(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.ensureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	apiSrv := httptest.NewServer(fs.routes())
	defer apiSrv.Close()
	api = apiSrv.URL

	dir := t.TempDir()
	v1 := writeRandom(t, filepath.Join(dir, "v1.bin"), 20<<20+123) // 3 parts, odd-sized last part
	v2 := writeRandom(t, filepath.Join(dir, "v2.bin"), 9<<20)      // 2 parts
	for _, p := range []string{v1, v2} {
		if err := upload(p, "reports/big.bin"); err != nil {
			t.Fatalf("upload %s: %v", p, err)
		}
	}

	var versions []versionInfo
	if err := callAPI("GET", "/files/versions?"+url.Values{"key": {"reports/big.bin"}}.Encode(), nil, &versions); err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || !versions[0].IsLatest {
		b, _ := json.Marshal(versions)
		t.Fatalf("want 2 versions, newest first; got %s", b)
	}

	latest, oldest := filepath.Join(dir, "latest"), filepath.Join(dir, "oldest")
	if err := download("reports/big.bin", latest, ""); err != nil {
		t.Fatal(err)
	}
	if err := download("reports/big.bin", oldest, versions[1].VersionID); err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, v2, latest)
	assertSameFile(t, v1, oldest)
}

func Test_partSizeFor_staysUnderPartLimit(t *testing.T) {
	for _, size := range []int64{1, 8 << 20, 100 << 30, 5 << 40} {
		ps := partSizeFor(size)
		if ps < minPartSize || (size+ps-1)/ps > maxParts {
			t.Errorf("size %d: partSize %d gives %d parts", size, ps, (size+ps-1)/ps)
		}
	}
}

func writeRandom(t *testing.T, path string, n int) string {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertSameFile(t *testing.T, want, got string) {
	t.Helper()
	a, _ := os.ReadFile(want)
	b, _ := os.ReadFile(got)
	if !bytes.Equal(a, b) {
		t.Fatalf("%s (%d bytes) != %s (%d bytes)", got, len(b), want, len(a))
	}
}
