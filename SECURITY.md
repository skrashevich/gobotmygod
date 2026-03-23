# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in BotMux, please report it responsibly.

**Do NOT open a public GitHub issue for security vulnerabilities.**

Instead, please email: **svk🌀svk.su** or **s.krashevich@🌀gmail.com**

You should receive a response within 168 hours. If the issue is confirmed, a fix will be released as soon as possible.

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest release | Yes |
| Older releases | No |

We recommend always running the latest version.

## Security Considerations

- **Bot tokens** are sensitive credentials. Use environment variables or secure flag passing.
- **SQLite database** (`botdata.db`) contains all collected messages. Restrict file access appropriately.
- **Default admin credentials** (`admin`/`admin`) must be changed on first login.
- **API keys** (`bmx_` prefix) should be treated as secrets and rotated periodically.
- For production deployments, always use HTTPS via a reverse proxy (nginx, Caddy, etc.).
- The `/tgapi/` proxy endpoint has no authentication by design (backends use bot tokens for auth). Restrict network access to this endpoint in production.