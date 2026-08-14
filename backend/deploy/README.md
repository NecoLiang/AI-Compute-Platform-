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

The backend joins `config_default` for MySQL/Redis and
`wanxiang-frontend_default` for the frontend. Port 8080 is bound only to
`127.0.0.1`; the frontend container can use
`http://wanxiang-backend:8080/api/v1`.
