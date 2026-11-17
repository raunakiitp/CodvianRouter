# Security Policy

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Please report vulnerabilities privately, using one of these routes:

1. **GitHub private vulnerability reporting** — on this repository, go to
   *Security → Report a vulnerability* and submit a private security
   advisory. This is the preferred route.
2. If private vulnerability reporting is unavailable, contact the
   maintainers through the Codvian website's contact route and mark the
   message as a security report for Codvian Router.

Please include: what the issue is, where it is, steps to reproduce, and
the impact you see. We will acknowledge the report, investigate, and fix
confirmed issues before disclosing them. Please give us reasonable time
to fix the issue before any public disclosure.

## Security principles

Codvian Router handles your provider API keys, so security is a
first-class feature, not an afterthought:

- **Auth on by default.** The gateway requires authentication out of the
  box. There is no mode where it silently runs open.
- **No default credentials.** There is no built-in password or API key.
  You create your own gateway key on first setup.
- **Localhost-only by default.** The gateway binds to localhost unless
  you explicitly configure it otherwise. Exposing it to a network is a
  deliberate, visible choice.
- **Keys stay local.** Provider API keys are stored on the machine
  running the router. They are never sent to Codvian or any third party.
- **No telemetry by default.** The router does not phone home with usage
  data. Any future opt-in telemetry would be explicit and off by default.
- **Secrets are never committed.** See `.gitignore` — `.env` files, API
  keys, and credentials must never enter the repository. CI and reviewers
  check for this.

## Supported versions

The project is in development and has no stable release yet. Security
fixes will be published against the latest `main` until the first stable
release, after which a supported-versions table will be added here.
