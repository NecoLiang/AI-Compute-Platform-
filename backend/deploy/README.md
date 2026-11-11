# Backend deployment

GitHub Actions builds `backend/`, transfers the tagged Docker image to
`123.56.177.241`, smoke-tests a candidate, and switches
`wanxiang-backend` with rollback on failure.

Repository secrets required:

- `DEPLOY_SSH_KEY`: private key for `root@123.56.177.241`.
- `DEPLOY_KNOWN_HOSTS`: pinned `known_hosts` entry for the server.

The server owns `/opt/wanxiang/backend/.env`; Actions never replaces it.
It must contain non-empty `DATABASE_DSN`, `REDIS_PASSWORD`,
`JWT_ACCESS_SECRET`, `JWT_REFRESH_SECRET`, and `SECURITY_CREDENTIAL_KEY`.
Run `bootstrap-server.sh` once as root to derive the database/Redis settings
from `/opt/wanxiang/config/.env.middleware` and generate independent JWT
secrets. It refuses to rotate an existing backend environment.

To enable Alibaba Cloud SMS login and registration, append these values to
the server-owned `.env` after the SMS signature, verification-code template,
and carrier registration have been approved:

```dotenv
SMS_ENABLED=true
SMS_SIGN_NAME=approved-sign-name
SMS_LOGIN_TEMPLATE_CODE=SMS_123456789
SMS_REGISTER_TEMPLATE_CODE=SMS_987654321
SMS_CODE_TTL=300
ALIBABA_CLOUD_ECS_METADATA=ComputeExchangeSmsRole
ALIBABA_CLOUD_IMDSV1_DISABLED=true
```

Prefer an ECS RAM role with only `dysms:SendSms`. The SDK uses Alibaba
Cloud's default credential chain. If an instance role is unavailable, inject
`ALIBABA_CLOUD_ACCESS_KEY_ID` and `ALIBABA_CLOUD_ACCESS_KEY_SECRET` through
the server `.env`; never commit them. `SendSms` is billable and is not called
by deployment smoke tests.

The backend joins `config_default` for MySQL/Redis and
`wanxiang-frontend_default` for the frontend. Port 8080 is bound only to
`127.0.0.1`; the frontend container can use
`http://wanxiang-backend:8080/api/v1`.

## Managed rental (fleet) images and access gateway

The backend also creates the internal network `omnis-accessgw-internal`
(172.30.21.0/24, no outbound route) and mounts `/opt/wanxiang/backend/secrets`
read-only at `/etc/omnis` (the fleet self-test key). Both are inert while
`FLEET_ENABLED` is unset/false.

`.github/workflows/fleet-images.yml` builds the four platform images
(`images/probe`, `images/lease-base`, `images/model-pull`, `accessgw/`),
pushes them to `ghcr.io/<owner>/omnis-fleet-<name>:<commit>` and, when the
`ACR_REGISTRY`, `ACR_NAMESPACE`, `ACR_USERNAME` and `ACR_PASSWORD` secrets
exist, to Alibaba Cloud ACR as well. It then runs the production command
templates against the pushed images on a GPU-less runner
(`TestFleetImagesSmoke`, `TestModelPullImageSmoke`). The job summary lists the
`ref@sha256` values to put in the fleet configuration; configuration only
accepts digests.

The access gateway runs on the same ECS behind its own egress IP; see
`accessgw/README.md` for the console prerequisites, `accessgw/setup.sh` and
the variables needed to enable fleet.
