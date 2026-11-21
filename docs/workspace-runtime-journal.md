# Workspace Runtime Journal

Status: S3 CAS store and local tests implemented; no Resource Plane deployment
or AWS bucket policy has been applied.

## Purpose and boundary

The journal is the Resource Plane's durable correctness boundary for Workspace
provider side effects. It records the highest accepted Workspace revision,
the currently pinned Lambda MicroVM allocation, the immutable launch token and
parameter digest, cleanup progress, and deletion tombstones. It contains no
session token, AWS credentials, endpoint auth token, or secret value.

The journal is not a second Control Plane database. Control Plane DSQL remains
the system of record for desired/observed Workspace state and resource
bindings; the Resource Plane must never connect to it. A provider binding is
reported back through the authenticated result protocol. The RP journal is
consulted first to fence and recover work after controller restarts.

## Object layout and compare-and-swap

The `S3Store` object key is:

```text
<configured-prefix>/<resource-plane-id>/<tenant-id>/<workspace-id>.json
```

Creation uses `PutObject If-None-Match: *`; updates require the exact ETag read
with the current journal and use `PutObject If-Match: <etag>`. A stale ETag or
concurrent conditional-write conflict is returned to the caller; it must reload
the journal and re-evaluate the command, never blindly overwrite. The serialized
record is capped at 32 KiB, validates schema/state/identity, rejects unknown
JSON fields, and verifies that the requested object key matches its payload.

The journal has two monotonic counters: `workspace_revision` fences delayed
Control Plane commands, while `revision` is the S3 journal CAS version. The
current allocation generation may advance only after the previous provider
allocation is confirmed terminated and a new immutable launch token/spec digest
is recorded. A provider ID cannot be replaced within one allocation generation.

Delete persists an irrevocable tombstone before provider termination. A delete
for a Workspace with no prior allocation writes a completed tombstone directly.
Cleanup failure remains a tombstone with `cleanup_state=failed`; retries may
move it back to pending, but no later ensure/resume can clear the tombstone.
Never expire or delete journal objects as part of ordinary Runtime cleanup.

## Resource Plane IAM and bucket requirements

The eventual Resource Plane runtime role should receive only `s3:GetObject` and
`s3:PutObject` for the configured journal prefix and its Resource Plane key
space. It does not need `s3:ListBucket`, `s3:DeleteObject`, or any DSQL action.
If SSE-KMS is selected, grant only the specific key permissions required by S3
(`kms:GenerateDataKey` and `kms:Decrypt`) and scope the key policy to this role.
Use bucket-default encryption, block public access, TLS-only bucket policy,
versioning for operator recovery, and a deny on runtime-role object deletion.
Do not configure lifecycle expiration for active or tombstoned journal records.

The runtime role should be scoped to the exact Resource Plane prefix, not a
shared bucket-wide wildcard. The S3 bucket and role provisioning belong to the
Resource Plane deployment task; this change only defines the code and policy
contract.

## Local verification

```bash
GOCACHE=/tmp/hako-go-cache go test ./internal/workspacejournal -count=1
```

The local S3 stub tests conditional create, concurrent ETag CAS, key/payload
identity checks, oversized/malformed records, and recovery after an ambiguous
write across a simulated controller restart. They do not prove AWS S3/IAM
configuration or Lambda MicroVM behavior.

## References

- [S3 PutObject conditional headers](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html)
- [S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)
- [Workspace Lambda MicroVM contract](workspace-lambda-microvm.md)
