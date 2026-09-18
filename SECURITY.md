# Security policy

## Reporting a vulnerability

Please report security vulnerabilities privately through GitHub's **Report a vulnerability** feature for this repository. Do not include credentials, private messages, database contents, or other sensitive data in a public issue.

Include the affected version or commit, a minimal reproduction, expected impact, and any suggested mitigation. Maintainers will acknowledge the report, investigate it, and coordinate disclosure when a fix is available.

## Supported versions

Until the first stable release, security fixes target the latest published release and the default branch. Older prereleases may require upgrading.

## Deployment boundary

memgov stores local state in SQLite and can connect to external tools configured by the operator. Operators are responsible for protecting the data directory, configuration files, backups, runtime account, and third-party credentials. See [governance and recovery](docs/guides/governance.md) for data-handling guidance.
