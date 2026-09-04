# Simulated project for local CI/CD validation

Seeded by `deploy/gitlab/provision.sh` into the standalone GitLab sandbox
(`deploy/gitlab/docker-compose.yaml`). Pipeline definitions live in
`.gitlab-ci.yml`; see `deploy/gitlab/README.md` for the two-pipeline layout
(dev-flow -> test-flow promotion).
