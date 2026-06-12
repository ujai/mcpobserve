# Security Policy

## Supported Versions

Only the latest tagged release is supported for security fixes.

| Version | Supported |
| --- | --- |
| Latest tag | Yes |
| Older tags and unreleased commits | No |

## Reporting

Report vulnerabilities privately to `mr.abudzar@pm.me`.

Do not open public GitHub issues, discussions, or pull requests for suspected
security vulnerabilities.

Reports should include:
- affected version or commit
- impact summary
- reproduction steps or proof of concept, if available

An acknowledgement will be sent within 72 hours.

## Threat Model

`mcpobserve` is a local observability proxy for MCP stdio servers. It starts a
configured MCP server as a child process, forwards stdio bytes in both
directions without application-layer inspection, and exposes Prometheus metrics
on `127.0.0.1:9464` by default.

The main security concerns are:
- unintended exposure of the metrics endpoint beyond localhost
- unsafe execution of the configured child process or its arguments
- leakage of sensitive data through process environment, logs, or metrics labels
- local privilege boundary mistakes when integrating with untrusted MCP servers

## Scope Notes

This policy covers security issues in this repository's source code, release
artifacts, and default runtime behavior.

It does not cover vulnerabilities in downstream deployments, host operating
systems, third-party MCP servers started by the proxy, or user-specific
configuration mistakes outside the documented defaults.
