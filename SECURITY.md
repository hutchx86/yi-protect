# Security policy

## Supported versions

Security fixes land on the latest release and on `main`. Older releases are not patched
unless the README says otherwise.

## Reporting a vulnerability

Do not open a public issue. Use GitHub's private reporting:

1. Open the repository's **Security** tab.
2. Choose **Report a vulnerability**.
3. Include the affected version or commit, hardware and firmware, reproduction steps,
   and impact.

If private reporting is unavailable, contact the maintainer through their GitHub profile
and ask for a private channel.

## What to expect

This is a best-effort project, not a commercial product. Expect an acknowledgement
within a few days and a fix or mitigation once the report is confirmed. There is no bug
bounty and no guaranteed response time.

## Scope

In scope: authentication or authorisation bypass, command injection, path traversal,
secret exposure, denial of service reachable from the network, and unsafe defaults.

Out of scope: damage from running the project against hardware you do not own,
misconfiguration described in the README, and findings that require physical access to
the host.

## Disclosure

Please give us time to ship a fix before publishing details. Credit is given in the
release notes unless you prefer to stay anonymous.
