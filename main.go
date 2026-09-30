// Command s3mpu is a learning demo of S3 multipart upload via presigned URLs.
//
//	s3mpu serve                              run the file-service API (control plane only)
//	s3mpu upload   <file> <key>              multipart upload straight to S3
//	s3mpu download <key> <out> [versionId]   presigned GET, latest or a given version
//	s3mpu versions <key>                     list all versions of a key
//	s3mpu delete   <key> [versionId]         soft delete (marker) or hard delete a version
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

const uploadConcurrency = 4

var api = envOr("API", "http://localhost:8080")

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 1 && args[0] == "serve":
		err = serve(envOr("ADDR", ":8080"))
	case len(args) == 3 && args[0] == "upload":
		err = upload(args[1], args[2])
	case len(args) >= 3 && args[0] == "download":
		err = download(args[1], args[2], optional(args, 3))
	case len(args) == 2 && args[0] == "versions":
		err = printJSON("GET", "/files/versions?"+url.Values{"key": {args[1]}}.Encode())
	case len(args) >= 2 && args[0] == "delete":
		err = printJSON("DELETE", "/files?"+url.Values{"key": {args[1]}, "versionId": {optional(args, 2)}}.Encode())
	default:
		fmt.Fprintln(os.Stderr, "usage: s3mpu serve | upload <file> <key> | download <key> <out> [versionId] | versions <key> | delete <key> [versionId]")
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// upload runs the 3-step protocol. Only steps 1 and 3 hit our API; the bytes
// in step 2 go directly from this process to S3.
func upload(path, key string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()

	// 1. Initiate: get uploadId + one presigned URL per part.
	var init initiateResponse
	if err := callAPI("POST", "/uploads", initiateRequest{Key: key, Size: size}, &init); err != nil {
		return fmt.Errorf("initiate: %w", err)
	}
	log.Printf("uploadId=%s parts=%d partSize=%dMiB", init.UploadID, len(init.URLs), init.PartSize>>20)

	// 2. PUT each part in parallel. Parts are independent, so each can be retried alone.
	parts := make([]completedPart, len(init.URLs))
	errs := make([]error, len(init.URLs))
	sem := make(chan struct{}, uploadConcurrency)
	var wg sync.WaitGroup
	for i, partURL := range init.URLs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			off := int64(i) * init.PartSize
			n := min(init.PartSize, size-off)
			etag, err := putPart(partURL, f, off, n)
			parts[i], errs[i] = completedPart{PartNumber: int32(i + 1), ETag: etag}, err
			if err == nil {
				log.Printf("part %d/%d done etag=%s", i+1, len(init.URLs), etag)
			}
		})
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		abortErr := callAPI("POST", "/uploads/abort", completeRequest{Key: key, UploadID: init.UploadID}, nil)
		return errors.Join(fmt.Errorf("upload parts: %w", err), abortErr)
	}

	// 3. Complete: S3 assembles the parts into a new object version.
	var done map[string]string
	if err := callAPI("POST", "/uploads/complete", completeRequest{Key: key, UploadID: init.UploadID, Parts: parts}, &done); err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	log.Printf("uploaded key=%s versionId=%s etag=%s", done["key"], done["versionId"], done["etag"])
	return nil
}

// putPart PUTs bytes [off, off+n) to a presigned URL and returns S3's ETag for
// that part. Browsers can only read this header if bucket CORS exposes "ETag".
func putPart(partURL string, f *os.File, off, n int64) (string, error) {
	var lastErr error
	for range 3 {
		req, err := http.NewRequest(http.MethodPut, partURL, io.NewSectionReader(f, off, n))
		if err != nil {
			return "", err
		}
		req.ContentLength = n
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp.Header.Get("ETag"), nil
		}
		lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return "", lastErr
}

func download(key, out, versionID string) error {
	q := url.Values{"key": {key}}
	if versionID != "" {
		q.Set("versionId", versionID)
	}
	var res map[string]string
	if err := callAPI("GET", "/files/download?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	resp, err := http.Get(res["url"]) // straight from S3
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("s3 get: status %d: %s", resp.StatusCode, body)
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	log.Printf("downloaded %d bytes to %s (versionId=%s)", n, out, resp.Header.Get("x-amz-version-id"))
	return nil
}

func callAPI(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func printJSON(method, path string) error {
	var v any
	if err := callAPI(method, path, nil, &v); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func optional(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}
