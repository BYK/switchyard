# AWS credentials for a worker outside AWS

Switchyard runs on `labs` and calls the AWS CLI to manage its London EC2 worker. A root-account `aws login` session lasts at most 12 hours. IAM Roles Anywhere replaces daily browser sign-in with short-lived role credentials issued to a machine certificate. The credential helper renews AWS credentials; it does **not** renew the certificate.

## Set up the identity

1. Use an existing private certificate authority (CA), or make a one-time CA to issue the `labs` certificate and delete its signing key immediately. The present installation used a private temporary directory on `labs` for that one-time key; it was deleted after issuance. The end-entity certificate has subject `CN=labs-switchyard`, issuer `CN=Switchyard-CA`, `digitalSignature` key usage, and `clientAuth` extended key usage. Keep its private key under `~/.config/switchyard/aws-identity/` on `labs` with a private directory and mode `0600` file. Only the public CA certificate goes to AWS. An offline CA or AWS Private CA is an alternative; a public CA cannot serve as a Roles Anywhere trust anchor.
2. In the AWS console, choose **London (`eu-west-2`) → IAM Roles Anywhere → Trust anchors → Create trust anchor**. Choose **External certificate bundle** and upload only the CA certificate in PEM format. Keep the new trust-anchor ARN.
3. In **IAM → Roles → Create role → Custom trust policy**, trust `rolesanywhere.amazonaws.com` for `sts:AssumeRole`, `sts:TagSession`, and `sts:SetSourceIdentity`. Restrict the trust policy to the exact trust-anchor ARN using `aws:SourceArn` and to **both** `aws:PrincipalTag/x509Subject/CN = labs-switchyard` and `aws:PrincipalTag/x509Issuer/CN = Switchyard-CA`. Use the *actual* issuer CN if it differs. Create a dedicated role, for example `SwitchyardWorker`, and keep its ARN.
4. Attach a policy scoped to London and the worker resources. The current `internal/switchyard/aws.go` calls `ssm:GetParameter` for the canonical Ubuntu 24.04 AMI and these EC2 actions: `DescribeInstances`, `DescribeKeyPairs`, `DescribeVpcs`, `DescribeSubnets`, `DescribeImages`, `DescribeSecurityGroups`, `DescribeVolumes`, `GetConsoleOutput`, `ImportKeyPair`, `DeleteKeyPair`, `CreateSecurityGroup`, `DeleteSecurityGroup`, `AuthorizeSecurityGroupIngress`, `RevokeSecurityGroupIngress`, `RunInstances`, `StartInstances`, `StopInstances`, and `TerminateInstances`. Tag-on-create needs `CreateTags`. The installed `SwitchyardWorkerOnly` policy limits launches to London, `m7i.xlarge`, encrypted tagged volumes, the worker's key and tagged security group, one default subnet, and the Canonical image owner's effective EC2 owner value. It limits instance, key, and group management to tagged Switchyard resources. EC2 authorization evaluates VPC access separately when creating a security group. Test the policy with the named profile below before withdrawing the old login.
5. In **IAM Roles Anywhere → Profiles → Create profile**, select the dedicated role. A session policy can narrow the role's permissions but cannot grant additional rights. Keep this profile ARN.

## Configure `labs`

Download AWS's **Linux x86-64 credential helper 1.8.5** from [the official helper guide](https://docs.aws.amazon.com/rolesanywhere/latest/userguide/credential-helper.html). Its published SHA-256 is `beec9ed1c492d93db809890f16713e3556353294b823c2184ad4e891f1b2b54d`. Check the digest before installing it at `/home/byk/.local/bin/aws_signing_helper`. The certificate and private key must never enter the repository or job snapshot.

Add a **named profile** to `~/.aws/config`, substituting the three ARNs from the console:

```ini
[profile switchyard]
region = eu-west-2
credential_process = /home/byk/.local/bin/aws_signing_helper credential-process --region eu-west-2 --certificate /home/byk/.config/switchyard/aws-identity/leaf.pem --private-key /home/byk/.config/switchyard/aws-identity/leaf.key --trust-anchor-arn TRUST_ANCHOR_ARN --profile-arn PROFILE_ARN --role-arn ROLE_ARN
```

Verify the profile without printing credentials or the AWS account number:

```sh
AWS_PROFILE=switchyard aws sts get-caller-identity --output json | python3 -c 'import json,sys; arn=json.load(sys.stdin)["Arn"]; print("dedicated_role", ":assumed-role/SwitchyardWorker/" in arn, "root", arn.endswith(":root"))'
switchyard aws auth select switchyard
switchyard aws auth status
switchyard aws status
```

The first check must show `dedicated_role True root False` if you kept the suggested role name. `switchyard aws auth select switchyard` verifies the dedicated assumed role against the tracked worker account, then saves the selected profile in Switchyard's private worker state. Every later Switchyard AWS call uses that profile, even when the OpenCode service has no `AWS_PROFILE`. A conflicting `AWS_PROFILE` fails closed; parent AWS credential variables cannot override the selected role. The selection also survives worker destruction. If your dedicated role has another name, pass `--role ROLE` when selecting it. Test `aws up`, `setup`, a previewed job, `down`, and a confirmed `stopped` status before withdrawing the old login. This selection changes only Switchyard, requires no `sudo` or OpenCode restart, and never sets a profile for `lore-gateway.service`. The helper obtains fresh AWS credentials while the end-entity certificate remains valid; renew that certificate before it expires.

The `labs` identity was issued on **2026-10-10**. Its certificate expires **2027-10-10 at 07:45 UTC**. The CA signing key was deleted after issuance, so the helper cannot renew this certificate. **Start rotation by 2027-09-10**: issue a new one-time CA and client certificate, create a second trust anchor, allow both anchors in the role trust policy during the overlap, update the named profile to the new certificate and anchor, verify a fresh role session and worker lifecycle, then disable/delete the old anchor and certificate. Never put the CA signing key or client key in this repository.

Do not put `AWS_PROFILE` in `/home/byk/.opencode/env`: `lore-gateway.service` also reads that file. The private Switchyard selection replaces the previously proposed systemd drop-in.

Do not use `aws_signing_helper update` for this setup: it writes usable temporary credentials to disk. Use the named `credential_process` profile instead. Do not use the helper's local `serve` endpoint on a multi-user machine; processes that can reach its loopback address can request credentials.
