# Resource Plane messaging module

Creates encrypted SQS command/Workspace Operation result queues and their
DLQs, redrive and redrive-allow policies, exact cross-account queue policies,
a least-privilege Resource Controller Lambda execution role, and a retained
CloudWatch log group. Separate Workload command and result queues/DLQs and a
Workload Controller role can be opted in with an exact Control Plane consumer
role ARN. A Worker Lambda remains disabled unless the separate
`workload_controller_deployment` object is provided.
Runtime IAM remains disabled unless `workload_controller_runtime` is configured
with concrete image, bucket/prefix, and lease ARNs plus six unique Secrets
Manager ARNs; only then are those exact Secrets readable and runtime actions
granted. Terraform never accepts Secret values. The manifest must pair the
Workload command/result queue URLs, and
the Dispatcher never falls back to the Workspace command queue. An explicit opt-in can create a development-only Fake Runtime Lambda
and SQS event source mapping. CloudWatch alarms cover DLQ messages, primary
queue age/backlog, and (when enabled) Fake Controller invocation errors.
Supplying `workload_controller_deployment` also creates the ARM64 combined
Workload Controller Lambda, a batch-size-1 partial-response mapping from only
the dedicated Workload command queue, a bounded EventBridge recovery schedule,
an independently scheduled bounded Artifact cleanup run, and Lambda/EventBridge
invocation alarms. The Worker role gets `s3:DeleteObject` only on the configured
Artifact prefix; cleanup discovers candidates and acknowledges completion over
the signed Control Plane API, never by connecting to DSQL. It requires an existing ZIP, HTTPS
Control Plane/broker endpoints, six exact Secret ARNs matching the IAM input,
the exact MicroVM/S3 runtime policy, bounded recovery/cleanup settings, and at least
two reserved Lambda concurrency slots so a recovery invocation need not
consume the only command-processing slot. The object defaults to `null`; no Worker function, trigger, Secret access, or
MicroVM permissions are created by default. Secret values are never Terraform
inputs. Endpoint reachability, VPC/NAT/egress controls, bucket and KMS key
policies, image/git wiring, and live MicroVM isolation still need separate
review and validation. Alarm notification actions are optional SNS topic
ARNs; Gateway deployment is not included.

When enabled, Terraform resolves the route table associated with each supplied
Worker subnet and fails planning unless each has `0.0.0.0/0` targeting a NAT
Gateway. The module does not create or change routes or NAT. This checks only
the configured route target; NAT health, DNS, security-group egress, endpoint
policies, and Control Plane/broker reachability still require environment-level
verification.

See [`docs/resource-plane-queues-and-dlq.md`](../../../../docs/resource-plane-queues-and-dlq.md)
for queue semantics, IAM boundaries, operational limitations, and validation.

Required inputs: `resource_plane_id`,
`control_plane_dispatcher_role_arn`, and
`control_plane_result_consumer_role_arn`. The runtime object and all six
Resource Plane-local Secret ARNs are optional and must be populated together
only after reviewing the worker's runtime policy. All queue URLs/ARNs and the
controller role/log group and monitoring alarm names are exposed as outputs;
Workload queue and role outputs are null unless enabled. The Worker Lambda,
SQS mapping, and recovery schedule outputs remain null unless deployment
configuration is provided.

See [`docs/terraform-modules.md`](../../../../docs/terraform-modules.md) for the complete input/output inventory, resource tags, IAM boundaries, and dev environment differences.

Run `make terraform-test` for AWS-mock plan tests that verify the Worker stays
disabled by default and validate the full opt-in Lambda/queue/recovery graph.
These tests neither apply resources nor contact AWS.

`actions_runner_deployment` is a separate default-off Lambda MicroVM startup
worker. When supplied with a built ZIP, four distinct exact Secret ARNs, a
fixed image/version, egress connector, and private subnet/security-group
configuration, it creates an encrypted startup queue/DLQ, allows only the
configured Control Plane Dispatcher role to send, and attaches a batch-size-1
SQS event source mapping with `ReportBatchItemFailures`. Its Lambda role is
restricted to that queue, those Secrets, the selected MicroVM image and
connector, logs, and the provided VPC resources. The queue URL is exposed for
the Control Plane Resource Plane manifest. The manifest currently permits only
one Actions Runner queue until durable multi-Plane runner placement exists.
This remains opt-in and does not create a runner image or apply infrastructure.

`actions_runner_cleanup_deployment` is an independent opt-in on top of the
startup deployment. It creates a scheduled ARM64 Lambda using the same reviewed
ZIP in `cleanup` mode, with its own concurrency-one role, private subnet/SG
attachment, EventBridge schedule, invoke permission, and Lambda error alarm.
The cleanup role reads only the broker certificate/key/CA Secrets and grants
`lambda:GetMicrovm`/`lambda:TerminateMicrovm` against only the configured runner
image ARN; it has no SQS, launch, token-minting, or execution-role permissions.
Optional KMS decrypt keys are supplied explicitly for those broker Secrets.
The default schedule is `rate(5 minutes)`, with a four-job batch, 120-second
lease, and 60-second operation budget; validation ensures a full batch fits
inside the Lambda timeout. The schedule remains absent unless this variable is
explicitly set. No apply is performed by module tests.

`workspace_service_controller_deployment` is a separate default-off Lambda
MicroVM consumer for Workspace Service reconciliation. It provisions an
encrypted dedicated command queue/DLQ, exact Dispatcher send policy,
batch-size-1 partial-failure mapping, retained logs, and DLQ/age/Lambda-error
alarms. The consumer role can read only journal objects below the configured
Resource-Plane prefix, exactly two distinct ticket/report Secrets, and invoke
`GetMicrovm`/`CreateMicrovmAuthToken` only for the configured MicroVM image.
The Lambda is attached to caller-provisioned private subnets and a dedicated
security group; the module creates no VPC endpoints, NAT, routes, or guest
network rules. Supply HTTPS Control Plane reachability, MicroVM API access,
S3/Secrets endpoints, Secret/KMS policies, and runtime network policy as reviewed
infrastructure. Its IAM can create randomly generated credentials only below
`hako/workspaces/*/services/*/*`, with the expected Hako ownership tags, and
can describe/read only tagged Secrets in that namespace. Credential rotation
can add a staged version and move only the `AWSPENDING`/`AWSCURRENT` labels on
tagged Secrets in that same namespace. Deletion uses a seven-day recovery
window after Guest cleanup confirmation. See
[`workspace-service-secrets.md`](../../../docs/workspace-service-secrets.md).
`ec2:*` actions here are only Lambda VPC network-interface
integration permissions; EC2 is not a Hako workload runtime. No
`lambda:ConnectMicrovm` identity permission is granted. The object defaults to
`null`; queue URL, DLQ ARN, Lambda/mapping, role, and log group are exposed as
nullable outputs. Terraform accepts ARNs and a key identifier, never Secret
values; tests use a fake plan provider and do not retrieve Secrets or apply
resources.

AWS lists `GetMicrovm` and `TerminateMicrovm` against the `microvmImage`
resource type, so the cleanup actions are scoped to the configured image ARN
([Lambda Service Authorization Reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_lambda.html)).

Workspace EFS Resource Controller permissions are a separate opt-in input,
`workspace_storage_runtime`, defaulting to `null`. The dev Resource Plane root
sets it only when `workspace_storage.enable_dynamic_workspace_roles` is true,
using the exact EFS ID and ARN from the Workspace Storage module. The policy
limits Access Point create/tag/describe/delete to that filesystem, this
Resource Plane's ownership tags, and the Resource Plane account/Region. IAM
role creation is constrained by the exact `/hako/workspaces/*` path and
creation-time ownership tags; existing-role operations require matching
Resource Plane ownership tags. `iam:PassRole` is limited to that path and
`lambda.amazonaws.com`, without a ResourceTag condition. The policy grants no
trust-policy update or managed-policy attachment actions. The runtime role
provider still verifies exact Lambda trust and EFS inline policy before reuse
or deletion. Mock-provider tests verify default-off behavior and these scopes;
they do not prove live IAM authorization or substitute for deployment review.

The HTTPS/WSS Workspace Gateway is separately default-off through
`workspace_gateway_deployment`. When configured, the module creates a public
443-only HTTPS ALB (60-minute idle timeout for long-lived WebSockets), HTTPS
target group, private ARM64 Fargate service with a
digest-pinned image, CPU target tracking, health/ALB 5xx alarms, and a dedicated
task role. The Gateway can read only its Resource Plane journal prefix, call
`GetMicrovm` and `CreateMicrovmAuthToken` for one exact Lambda MicroVM image,
and read six exact operator-provisioned Secrets. A pinned AWS CLI sidecar loads
those SecretString values into an ephemeral shared task volume; the non-root
Gateway container mounts that volume read-only and never receives Secret values
in its environment. The task security group accepts 8443 only from its ALB and
permits DNS plus caller-listed HTTPS egress CIDRs. The user supplies public and
private subnets, ACM certificate, exact ECR image digest, HTTPS Control Plane
endpoints, runtime image, journal, and Secret/KMS ARNs. `workspace_gateway_url`
is the configured public origin; `workspace_gateway_alb_dns_name` is the ALB
address that DNS must target. The secret-loader image is restricted to a
digest-pinned Public ECR image so the task execution role does not need another
private repository permission. ECS tasks have no public IP and Execute Command
is disabled. This contract remains unapplied; DNS, routes/NAT/endpoints,
certificate/DNS ownership, Secret resource/KMS policies, and live Lambda
MicroVM WSS connectivity require separate review and runtime verification.

`workload_artifact_storage` optionally provisions the configured artifact
bucket when the exact Workload runtime scope and Workload queues are enabled.
It applies BucketOwnerEnforced ownership, all four S3 public-access blocks,
versioning, default SSE-S3 (or the explicitly configured artifact KMS key),
TLS-only transport, and a bucket-policy grant limited to the Workload
Controller role and artifact prefix. Lifecycle expiration removes current and
noncurrent versions after `retention_days`, removes expired delete markers,
and aborts incomplete multipart uploads after one day. Bucket destruction is
not forced. Leave this object null to use an externally managed bucket; the
module does not take ownership of or alter an external bucket implicitly.
The output bucket ARN/name are null when disabled. Existing buckets must be
imported and reviewed before switching to managed mode; Terraform does not
adopt an existing bucket automatically.
