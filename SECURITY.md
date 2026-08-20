# Security Policy

## Supported versions

The latest release line receives security fixes. Pin the `v1` major tag (or a
specific version) rather than a branch.

| Version | Supported |
| ------- | --------- |
| 1.x     | yes       |
| < 1.0   | no        |

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's private vulnerability
reporting: open the repository's **Security** tab and choose **Report a
vulnerability**. Do not open a public issue for anything you believe is a
security problem.

You can expect an acknowledgement within five business days. Please include a
reproduction if you can - a config file and target page are usually enough.

## Scope notes

Frostfall drives a headless browser against pages you point it at. Things that
are in scope for reports:

- The GitHub Action executing untrusted input (workflow inputs, config values)
- The release install path (checksum bypass, tag confusion)
- The embedded static file server (`--serve`) escaping the served directory
- The GitHub issue-filing client mishandling tokens

Scanning a malicious page with Frostfall runs that page's JavaScript in the
browser, exactly as visiting it would (note that the browser may run without
its OS sandbox in containerized CI); that alone is not a vulnerability, but a
malicious page escaping into the frostfall process or the filesystem would be.
