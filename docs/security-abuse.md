# Security And Abuse Notes

## Current Risk

Direct-exit relays are simple, but they put relay operators in the
destination-visible path. Destination services, abuse desks, and network
operators may see the relay's public IP as the source. For a volunteer-run
relay, that is typically the volunteer's own residential or server IP.

Before any public rollout, add controls for:

- Per-relay bandwidth and session limits.
- Destination blocklists for obvious abuse categories.
- Emergency relay disablement.
- Volunteer opt-in terms and warnings.
- Broker-side registration authentication.
- Health and reachability verification.
- Client and relay version enforcement.

## Broker Hardening

The broker should eventually use:

- Persistent storage.
- Signed relay descriptors.
- Per-client ephemeral VLESS credentials instead of one shared client ID per relay process.
- Relay registration authentication and operator reputation.
- Rate limiting.
- Audit logs for control-plane events.
- Separate public client API and private relay-registration API.

The broker should avoid storing:

- Browsing destinations.
- Client traffic metadata.
- Full client IP logs unless operationally required.

## Relay Hardening

The relay CLI (currently `cmd/volunteer`) should eventually support:

- OS-level firewall policy generation.
- Bandwidth ceilings.
- Session ceilings.
- Automatic updates.
- Clear exit mode display.
- Dedicated exit-server forwarding mode.

## Dedicated Exit Mode

Dedicated exit mode should become the preferred public mode:

```text
Censored client -> volunteer-run entry relay -> dedicated exit server -> public website/app
```

This reduces volunteer exposure and enables centralized abuse handling at the dedicated exit layer.
