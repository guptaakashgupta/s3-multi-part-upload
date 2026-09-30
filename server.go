package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	minPartSize   = 5 << 20 // S3 rule: every part except the last must be >= 5 MiB
	maxParts      = 10000   // S3 rule: at most 10,000 parts per upload
	maxObjectSize = 5 << 40 // S3 rule: 5 TiB max object
	presignTTL    = 15 * time.Minute
)

// fileService never sees file bytes. It only talks to S3 control-plane APIs
// and hands out short-lived presigned URLs; clients move the data directly.
type fileService struct {
	s3      *s3.Client
	presign *s3.PresignClient
	bucket  string
}

func newFileService(ctx context.Context) (*fileService, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if ep := os.Getenv("S3_ENDPOINT"); ep != "" { // MinIO/LocalStack; unset for real AWS
			o.BaseEndpoint = aws.String(ep)
			o.UsePathStyle = true
		}
		// Without this, newer SDKs sign a CRC32 of an empty body into presigned
		// PUT URLs, and the real upload then fails the checksum.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &fileService{s3: client, presign: s3.NewPresignClient(client), bucket: envOr("BUCKET", "files")}, nil
}

// ensureBucket creates the bucket and turns on versioning. Versioning is what
// makes "same key, many versions" work: each PUT/Complete gets a new VersionId.
// ponytail: fine for a demo; in prod the bucket belongs in Terraform, not app startup.
func (fs *fileService) ensureBucket(ctx context.Context) error {
	_, err := fs.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &fs.bucket})
	var owned *types.BucketAlreadyOwnedByYou
	if err != nil && !errors.As(err, &owned) {
		return fmt.Errorf("create bucket: %w", err)
	}
	_, err = fs.s3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  &fs.bucket,
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	if err != nil {
		return fmt.Errorf("enable versioning: %w", err)
	}
	return nil
}

func (fs *fileService) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /uploads", fs.initiateUpload)
	mux.HandleFunc("POST /uploads/complete", fs.completeUpload)
	mux.HandleFunc("POST /uploads/abort", fs.abortUpload)
	mux.HandleFunc("GET /files/versions", fs.listVersions)
	mux.HandleFunc("GET /files/download", fs.downloadURL)
	mux.HandleFunc("DELETE /files", fs.deleteFile)
	return mux
}

type initiateRequest struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

type initiateResponse struct {
	UploadID string   `json:"uploadId"`
	PartSize int64    `json:"partSize"`
	URLs     []string `json:"urls"` // URLs[i] is for PartNumber i+1
}

// Step 1: CreateMultipartUpload, then presign one UploadPart URL per part.
func (fs *fileService) initiateUpload(w http.ResponseWriter, r *http.Request) {
	var req initiateRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Key == "" || req.Size <= 0 || req.Size > maxObjectSize {
		httpError(w, http.StatusBadRequest, "key required and size must be 1..5TiB")
		return
	}
	partSize := partSizeFor(req.Size)
	numParts := int32((req.Size + partSize - 1) / partSize)

	out, err := fs.s3.CreateMultipartUpload(r.Context(), &s3.CreateMultipartUploadInput{Bucket: &fs.bucket, Key: &req.Key})
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	resp := initiateResponse{UploadID: *out.UploadId, PartSize: partSize, URLs: make([]string, numParts)}
	for i := range numParts {
		p, err := fs.presign.PresignUploadPart(r.Context(), &s3.UploadPartInput{
			Bucket: &fs.bucket, Key: &req.Key, UploadId: out.UploadId, PartNumber: aws.Int32(i + 1),
		}, s3.WithPresignExpires(presignTTL))
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp.URLs[i] = p.URL
	}
	writeJSON(w, resp)
}

// partSizeFor picks the smallest part size >= 8 MiB that keeps us under 10,000 parts.
func partSizeFor(size int64) int64 {
	partSize := int64(8 << 20)
	for size/partSize >= maxParts {
		partSize *= 2
	}
	return max(partSize, minPartSize)
}

type completedPart struct {
	PartNumber int32  `json:"partNumber"`
	ETag       string `json:"etag"`
}

type completeRequest struct {
	Key      string          `json:"key"`
	UploadID string          `json:"uploadId"`
	Parts    []completedPart `json:"parts"`
}

// Step 3: client reports each part's ETag (returned by S3 on every part PUT);
// S3 stitches the parts into one object and assigns it a new VersionId.
func (fs *fileService) completeUpload(w http.ResponseWriter, r *http.Request) {
	var req completeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Key == "" || req.UploadID == "" || len(req.Parts) == 0 {
		httpError(w, http.StatusBadRequest, "key, uploadId and parts required")
		return
	}
	slices.SortFunc(req.Parts, func(a, b completedPart) int { return int(a.PartNumber - b.PartNumber) }) // S3 requires ascending order
	parts := make([]types.CompletedPart, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = types.CompletedPart{PartNumber: aws.Int32(p.PartNumber), ETag: aws.String(p.ETag)}
	}
	out, err := fs.s3.CompleteMultipartUpload(r.Context(), &s3.CompleteMultipartUploadInput{
		Bucket: &fs.bucket, Key: &req.Key, UploadId: &req.UploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]string{"key": req.Key, "versionId": aws.ToString(out.VersionId), "etag": aws.ToString(out.ETag)})
}

// Abort frees the already-uploaded parts. Parts of an unfinished upload are
// billed storage and invisible in normal listings, so always abort on failure
// (and add a lifecycle rule AbortIncompleteMultipartUpload as a backstop).
func (fs *fileService) abortUpload(w http.ResponseWriter, r *http.Request) {
	var req completeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Key == "" || req.UploadID == "" {
		httpError(w, http.StatusBadRequest, "key and uploadId required")
		return
	}
	_, err := fs.s3.AbortMultipartUpload(r.Context(), &s3.AbortMultipartUploadInput{Bucket: &fs.bucket, Key: &req.Key, UploadId: &req.UploadID})
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type versionInfo struct {
	VersionID    string    `json:"versionId"`
	IsLatest     bool      `json:"isLatest"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
	DeleteMarker bool      `json:"deleteMarker,omitempty"`
}

func (fs *fileService) listVersions(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		httpError(w, http.StatusBadRequest, "key required")
		return
	}
	// ponytail: single page (up to 1000 versions); paginate with KeyMarker/VersionIdMarker if files get more.
	out, err := fs.s3.ListObjectVersions(r.Context(), &s3.ListObjectVersionsInput{Bucket: &fs.bucket, Prefix: &key})
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	versions := []versionInfo{}
	for _, v := range out.Versions {
		if aws.ToString(v.Key) == key { // Prefix also matches "key-other"
			versions = append(versions, versionInfo{aws.ToString(v.VersionId), aws.ToBool(v.IsLatest), aws.ToInt64(v.Size), aws.ToTime(v.LastModified), false})
		}
	}
	for _, d := range out.DeleteMarkers {
		if aws.ToString(d.Key) == key {
			versions = append(versions, versionInfo{aws.ToString(d.VersionId), aws.ToBool(d.IsLatest), 0, aws.ToTime(d.LastModified), true})
		}
	}
	slices.SortFunc(versions, func(a, b versionInfo) int { return b.LastModified.Compare(a.LastModified) })
	writeJSON(w, versions)
}

// Download is a presigned GET. Omit versionId for latest, pass it for an old version.
func (fs *fileService) downloadURL(w http.ResponseWriter, r *http.Request) {
	key, versionID := r.URL.Query().Get("key"), r.URL.Query().Get("versionId")
	if key == "" {
		httpError(w, http.StatusBadRequest, "key required")
		return
	}
	in := &s3.GetObjectInput{Bucket: &fs.bucket, Key: &key}
	if versionID != "" {
		in.VersionId = &versionID
	}
	p, err := fs.presign.PresignGetObject(r.Context(), in, s3.WithPresignExpires(presignTTL))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"url": p.URL})
}

// Delete without versionId adds a "delete marker" (soft delete; old versions
// stay and can be restored). With versionId it permanently removes that version.
func (fs *fileService) deleteFile(w http.ResponseWriter, r *http.Request) {
	key, versionID := r.URL.Query().Get("key"), r.URL.Query().Get("versionId")
	if key == "" {
		httpError(w, http.StatusBadRequest, "key required")
		return
	}
	in := &s3.DeleteObjectInput{Bucket: &fs.bucket, Key: &key}
	if versionID != "" {
		in.VersionId = &versionID
	}
	out, err := fs.s3.DeleteObject(r.Context(), in)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"versionId": aws.ToString(out.VersionId), "deleteMarker": aws.ToBool(out.DeleteMarker)})
}

func serve(addr string) error {
	ctx := context.Background()
	fs, err := newFileService(ctx)
	if err != nil {
		return err
	}
	if err := fs.ensureBucket(ctx); err != nil {
		return err
	}
	log.Printf("file-service listening on %s (bucket %q)", addr, fs.bucket)
	// ponytail: no authn/authz. A real service must check the caller may touch `key` before presigning.
	return http.ListenAndServe(addr, fs.routes())
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	log.Printf("%d: %s", code, msg)
	http.Error(w, msg, code)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
