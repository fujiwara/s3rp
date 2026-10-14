# 0010. The definition store is read-only; writes belong to the control plane

- Status: Accepted (recorded retroactively)
- Date: 2026-08-02
- Pull requests: [#21](https://github.com/fujiwara/s3rp/pull/21) (added),
  [#53](https://github.com/fujiwara/s3rp/pull/53) (sqlite store removed)

## Context

Tenants, users, keys and buckets have to come from somewhere. The proxy is
internet-facing; whatever it can write, an attacker who compromises it can
write too.

## Decision

- The gateway reads definitions through the read-only `store.Store`
  interface. All definition writes happen in a separate control plane with
  its own credentials and network; the proxy holds read-only access
  (SELECT-only for a database) and carries no write code.
- A sqlite store with a separate admin binary was built (#21) to prove the
  split, then removed (#53): as a library dependency sqlite was dead weight,
  and a definition store is exactly the part a service has to design for
  itself. The YAML config store remains as the PoC's stand-in.
- Validation that a store must run where definitions are written
  (`Backend.Validate`, name validators, `cors.Rule.Validate`, policy parsing)
  lives in the leaf packages, so every store runs the same checks.

## Alternatives considered

- **Keep a bundled database store.** Rejected for the reasons above.
- **Let the proxy write definitions.** Rejected: it widens what a
  compromised proxy can change.

## Consequences

- The control plane (admin API, credential issuance, lifecycle rule writes,
  usage aggregation) is out of this repository by design.
- A real store should cache parsed policies, since parsing per request
  repeats work the YAML store does once at startup.
