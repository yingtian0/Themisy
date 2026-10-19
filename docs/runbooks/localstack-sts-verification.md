# LocalStack STS broker verification (Issue #6)

`TestLocalStackSTSBrokerRoundTrip` is an opt-in integration test, not a production
credential path. It requires an already authorized, dedicated LocalStack instance
with STS/IAM available and the synthetic role below. It does not start a service,
read a customer credential, contact real AWS, or purchase a LocalStack license.

Current LocalStack for AWS images require an Auth Token. Provision an instance
through your existing authorized account using the
[official authentication procedure](https://docs.localstack.cloud/aws/getting-started/auth-token/).
Do not paste tokens into an Issue, terminal output or this repository. No token or
endpoint was available during implementation, so live LocalStack verification is
**unperformed**, not passed. Do not use older images to bypass authentication or
licensing requirements.

## Dedicated synthetic fixture

Use a literal loopback HTTP address with an explicit port, for example
`http://127.0.0.1:4566`. The Go test rejects DNS names, remote/cloud addresses,
missing ports, URL credentials, redirects, query strings and non-root paths. It
uses a proxy-free client, dummy `test` credentials and no default AWS credential
chain or automatic SDK retries. Its only AWS SDK operations are AssumeRole and
GetCallerIdentity against that endpoint.

Create the fixed role `themisy-localstack-sts-fixture` in the emulator's synthetic
account `000000000000`, using
[sts-trust-policy.json](../../test/fixtures/localstack/sts-trust-policy.json).
For an operator who already has an authorized instance and AWS CLI installed,
the following preparation command has an explicit local-only guard. It creates
only that fixture role, not an actual AWS role:

```bash
localstack_endpoint='http://127.0.0.1:4566'
case "$localstack_endpoint" in
  http://127.0.0.1:*) localstack_port="${localstack_endpoint#http://127.0.0.1:}" ;;
  *) echo 'Only a dedicated loopback LocalStack endpoint is allowed' >&2; exit 1 ;;
esac
case "$localstack_port" in
  ''|*[!0-9]*) echo 'Invalid local port' >&2; exit 1 ;;
esac
if [ "${#localstack_port}" -gt 5 ] || [ "$localstack_port" -lt 1 ] || [ "$localstack_port" -gt 65535 ]; then
  echo 'Invalid local port' >&2; exit 1
fi
env AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_SESSION_TOKEN='' \
  AWS_SECURITY_TOKEN='' AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null \
  AWS_PROFILE='' AWS_EC2_METADATA_DISABLED=true AWS_DEFAULT_REGION=us-east-1 \
  NO_PROXY=127.0.0.1 no_proxy=127.0.0.1 \
  HTTP_PROXY='' HTTPS_PROXY='' ALL_PROXY='' http_proxy='' https_proxy='' all_proxy='' \
  aws --endpoint-url "$localstack_endpoint" iam create-role \
    --role-name themisy-localstack-sts-fixture \
    --assume-role-policy-document file://test/fixtures/localstack/sts-trust-policy.json
```

An existing-role conflict requires read-only inspection of that dedicated fixture;
do not delete/replace roles or weaken the trust policy to make a test pass. This
preparation command was not executed during implementation. LocalStack's
[credential namespace documentation](https://docs.localstack.cloud/aws/connecting/credentials/)
explains its synthetic account mapping; this is not an AWS account credential.

## Run and retain evidence

```bash
THEMISY_LOCALSTACK_STS_ENDPOINT='http://127.0.0.1:4566' \
  make test-localstack-sts
```

The broker uses its fixed tenant/service/environment/deploy-role mapping with a
15-minute duration and External ID. The returned credential must be complete,
short-lived and usable for GetCallerIdentity with the expected role/session.
Unmapped tenant, ARN-as-service and rollback-purpose requests must make zero
additional AssumeRole calls. No response/credential values are logged. Existing
unit tests separately check session tags, purpose separation, redaction and
Runner authorization/Journal-before-acquisition ordering.

Record the exact commit, LocalStack version, synthetic fixture and test result.
The Make target refuses an absent endpoint with nonzero exit status. Directly
invoking the tagged Go test without its endpoint yields SKIP instead; that is not
live acceptance evidence. Endpoint safety tests also run in the ordinary package
suite without the integration tag. An unavailable health service, missing role,
incomplete credential or identity mismatch is failure; neither is a staging PASS.
Even a successful emulator run does not establish actual IAM trust enforcement,
workload identity isolation, CloudTrail attribution, real STS availability or ECS
behavior. Those dedicated AWS/operational gates remain #11; this test can supply
only #6's explicitly allowed LocalStack AssumeRole evidence once actually run.
