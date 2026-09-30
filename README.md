## S3 multipart upload with presigned URLs

A learning demo. The file-service API does control-plane work only; file bytes
go directly between the client and S3, so the API server never carries the
upload/download bandwidth.

```text
client                      file-service API                 S3
  | POST /uploads {key,size}      |                            |
  |------------------------------>| CreateMultipartUpload ---->|
  |                               |<---- uploadId -------------|
  |                               | presign UploadPart x N     |  (local signing, no network)
  |<-- uploadId, partSize, urls[] |                            |
  |                                                            |
  | PUT part 1..N (parallel, retry per part) ----------------->|
  |<------------------------------------------ ETag per part --|
  |                                                            |
  | POST /uploads/complete {parts[{partNumber,etag}]}          |
  |------------------------------>| CompleteMultipartUpload -->|
  |<-- versionId -----------------|<---- new VersionId --------|
  |                                                            |
  | GET /files/download?key[&versionId]                        |
  |------------------------------>| presign GetObject          |
  |<-- url -----------------------|                            |
  | GET url ------------------------------------------------->|
```

### Key rules

- Parts are 5 MiB to 5 GiB; only the last may be smaller. Max 10,000 parts and 5 TiB per object. `partSizeFor` picks the size.
- Each part PUT returns an `ETag`. Complete needs every `{partNumber, etag}` in ascending order.
- The multipart object ETag is `md5-of-part-md5s-N`, not the file's MD5. Don't use it as a content hash.
- If an upload is never completed, its parts are still stored and billed, and they don't appear in normal listings. Call Abort on failure, and add a bucket lifecycle rule `AbortIncompleteMultipartUpload` (for example after 7 days) as a backstop.
- Browser clients need bucket CORS that allows `PUT` and has `ExposeHeaders: ["ETag"]`, or JavaScript can't read the ETag.
- A presigned URL is a bearer token until it expires (15 minutes here). Check authorisation *before* presigning. This demo has no auth.

### Versioning

With bucket versioning enabled, each Complete (or PUT) to the same key creates a
new `VersionId`, and older versions are kept.

| Operation | Effect |
|---|---|
| GET without `versionId` | Latest version |
| GET with `versionId` | That exact version |
| DELETE without `versionId` | Adds a delete marker (soft delete); older versions remain |
| DELETE with `versionId` | Permanently removes that version, or that marker (which restores the file) |

Old versions are billed storage. In production, use a lifecycle rule
`NoncurrentVersionExpiration` (for example, keep the last N versions or expire
them after 90 days).

### HTTP API

Base URL is `http://localhost:8080`. Request and response bodies are JSON,
query parameters must be URL-encoded (`docs/report.bin` becomes
`docs%2Freport.bin`, though most clients accept the raw `/`), and timestamps
are RFC 3339 in UTC.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/uploads` | Start a multipart upload and get presigned part URLs |
| `PUT` | *presigned part URL* | Upload one part directly to S3 (not our API) |
| `POST` | `/uploads/complete` | Assemble the parts into a new object version |
| `POST` | `/uploads/abort` | Cancel an upload and free its parts |
| `GET` | `/files/versions?key=` | List all versions and delete markers of a key |
| `GET` | `/files/download?key=[&versionId=]` | Get a presigned download URL |
| `DELETE` | `/files?key=[&versionId=]` | Soft delete, or hard delete one version |

Errors come back as plain text: `400` for a bad request, `502` when S3
rejects the call (the S3 error message is passed through), and `500` if
presigning fails.

#### Start upload: `POST /uploads`

`size` is the total file size in bytes (1 byte to 5 TiB). The server picks the
part size and returns one URL per part; `urls[0]` is part 1.

```json
{ "key": "docs/report.bin", "size": 25000000 }
```

Response `200`:

```json
{
  "uploadId": "ZDE2MDEzODEtY2Y2ZS00ZjhlLWJiNGYt...",
  "partSize": 8388608,
  "urls": [
    "http://localhost:9000/files/docs/report.bin?partNumber=1&uploadId=ZDE2...&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=...&X-Amz-Expires=900&X-Amz-Signature=...",
    "http://localhost:9000/files/docs/report.bin?partNumber=2&uploadId=ZDE2...&X-Amz-Signature=...",
    "http://localhost:9000/files/docs/report.bin?partNumber=3&uploadId=ZDE2...&X-Amz-Signature=..."
  ]
}
```

#### Upload a part: `PUT <urls[i]>`

Send bytes `[i*partSize, min((i+1)*partSize, size))` as the raw body. Don't add
headers that weren't signed. Keep the `ETag` response header, including its
quotes.

```bash
dd if=v1.bin bs=8388608 skip=0 count=1 2>/dev/null > part1
curl -i -X PUT --upload-file part1 "curl -i -X PUT --upload-file part1 http://localhost:9000/files/docs/report.bin?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=localadmin%2F20260930%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260930T061605Z&X-Amz-Expires=900&X-Amz-SignedHeaders=host&partNumber=1&uploadId=ZDE2MDEzODEtY2Y2ZS00ZjhlLWJiNGYtMmRmNTM2M2U5YzA5LjhjYjc0YzczLTU5YmQtNDBmYi1iN2ZmLTNmZTAxYmZlZDg4NHgxNzkwNzQ4OTY1ODU3MzU4NTQ4&x-id=UploadPart&X-Amz-Signature=a7612088753f12b247b5375f649557c28c3e83f8719d4c6434698e16453862a7"
# HTTP/1.1 200 OK
# ETag: "0f616f9b48a84c7bbf26e57d17cd5152"
```

#### Complete: `POST /uploads/complete`

Send every part. The server sorts them by `partNumber` before calling S3.

```json
{
  "key": "docs/report.bin",
  "uploadId": "ZDE2MDEzODEtY2Y2ZS00ZjhlLWJiNGYt...",
  "parts": [
    { "partNumber": 1, "etag": "\"0f616f9b48a84c7bbf26e57d17cd5152\"" },
    { "partNumber": 2, "etag": "\"629a7caf1bc79244f4784bc6d7dbe6d4\"" },
    { "partNumber": 3, "etag": "\"67703fea8d2d857367a403e08e5976b9\"" }
  ]
}
```

Response `200`:

```json
{
  "key": "docs/report.bin",
  "versionId": "96ebe5e3-8f43-43ba-a980-316ffa62b1c4",
  "etag": "\"102825e8649f228b6a6bc465ce2bc115-3\""
}
```

#### Abort: `POST /uploads/abort`

```json
{ "key": "docs/report.bin", "uploadId": "ZDE2MDEzODEtY2Y2ZS00ZjhlLWJiNGYt..." }
```

Response `204 No Content`.

#### List versions: `GET /files/versions?key=docs/report.bin`

Newest first. Delete markers have `"deleteMarker": true` and `size: 0`.

```json
[
  {
    "versionId": "0736e1d4-ed4a-49d9-aa62-0de4824b2971",
    "isLatest": true,
    "size": 0,
    "lastModified": "2026-09-28T16:44:07.442Z",
    "deleteMarker": true
  },
  {
    "versionId": "cad7a955-0d7f-4e38-b96e-3b629b2cfafe",
    "isLatest": false,
    "size": 12000000,
    "lastModified": "2026-09-28T16:44:06.712Z"
  }
]
```

#### Download: `GET /files/download?key=docs/report.bin&versionId=cad7a955-...`

Leave out `versionId` to get the latest version. The URL is valid for 15
minutes; fetch it directly from S3.

```json
{ "url": "http://localhost:9000/files/docs/report.bin?versionId=cad7a955-...&X-Amz-Signature=..." }
```

```bash
curl -o out.bin "<url>"
```

If the latest version is a delete marker, the GET without `versionId` returns
`404` from S3.

#### Delete: `DELETE /files?key=docs/report.bin[&versionId=...]`

Without `versionId`, S3 adds a delete marker:

```json
{ "versionId": "0736e1d4-ed4a-49d9-aa62-0de4824b2971", "deleteMarker": true }
```

With `versionId`, that version is permanently removed. If the ID belongs to a
delete marker, removing it restores the previous version:

```json
{ "versionId": "cad7a955-0d7f-4e38-b96e-3b629b2cfafe", "deleteMarker": false }
```

#### Full flow with curl

```bash
API=http://localhost:8080
curl -s -X POST $API/uploads -d '{"key":"docs/report.bin","size":25000000}' > init.json
# PUT each part to its URL in init.json, collect the ETags, then:
curl -s -X POST $API/uploads/complete -d @complete.json
curl -s "$API/files/versions?key=docs/report.bin"
curl -s "$API/files/download?key=docs/report.bin"
curl -s -X DELETE "$API/files?key=docs/report.bin"
```

### Run it

```bash
docker compose up -d        # local S3-compatible server (RustFS) on :9000
export S3_ENDPOINT=http://localhost:9000 AWS_ACCESS_KEY_ID=localadmin \
       AWS_SECRET_ACCESS_KEY=localadmin AWS_REGION=us-east-1
go run . serve &            # API on :8080; creates bucket "files" with versioning

head -c 25000000 /dev/urandom > v1.bin
go run . upload v1.bin docs/report.bin
go run . upload v1.bin docs/report.bin      # second version
go run . versions docs/report.bin
go run . download docs/report.bin out.bin <versionId>
go run . delete docs/report.bin              # delete marker
```

For real AWS, leave `S3_ENDPOINT` unset and use your normal AWS credentials.
Set `BUCKET` to choose the bucket name.

### Test

```bash
go test ./...   # in-process fake S3; no Docker needed
```

The fake doesn't validate signatures. The Docker flow above does.
